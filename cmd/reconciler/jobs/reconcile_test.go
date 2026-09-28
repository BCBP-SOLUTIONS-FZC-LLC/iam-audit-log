package jobs

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
)

// fakeArchives is a minimal port.ArchiveRepository: every tier is already
// verified, so a run goes straight to the drop gate, which answers drop.
type fakeArchives struct {
	partitions []string
	listErr    error
	drop       string
	pruned     int64
	pruneErr   error
	ttl, batch int
}

func (f *fakeArchives) Partitions(context.Context) ([]string, error)       { return f.partitions, f.listErr }
func (f *fakeArchives) ReopenCandidates(context.Context) ([]string, error) { return nil, nil }
func (f *fakeArchives) Reopen(context.Context, string) (int64, error)      { return 0, nil }
func (f *fakeArchives) States(context.Context, string) (map[domain.RetentionTier]port.ArchiveTierState, error) {
	return map[domain.RetentionTier]port.ArchiveTierState{
		domain.TierSecurity3y: {Status: domain.ArchiveVerified}, domain.TierCompliance7y: {Status: domain.ArchiveVerified},
		domain.TierAccess90d: {Status: domain.ArchivePending},
	}, nil
}
func (f *fakeArchives) PendingRedaction(context.Context, string) (bool, error) { return false, nil }
func (f *fakeArchives) MarkArchiving(context.Context, string, domain.RetentionTier, time.Time) error {
	return nil
}
func (f *fakeArchives) NextParts(context.Context, string, domain.RetentionTier) (map[string]int, error) {
	return map[string]int{}, nil
}
func (f *fakeArchives) StreamTier(context.Context, string, domain.RetentionTier, func(domain.AuditEntry) error) error {
	return nil
}
func (f *fakeArchives) UpsertObject(context.Context, string, domain.ArchiveObject) error { return nil }
func (f *fakeArchives) FinishArchive(context.Context, string, domain.RetentionTier, []string, int64, string, string) error {
	return nil
}
func (f *fakeArchives) UnsealedObjects(context.Context, string, domain.RetentionTier) ([]domain.ArchiveObject, error) {
	return nil, nil
}
func (f *fakeArchives) MarkVerified(context.Context, string, domain.RetentionTier) error { return nil }
func (f *fakeArchives) MarkFailed(context.Context, string, domain.RetentionTier, string) error {
	return nil
}
func (f *fakeArchives) MarkAccessExpiring(context.Context, string, time.Time) error { return nil }
func (f *fakeArchives) ResetForRearchive(context.Context, string) error             { return nil }
func (f *fakeArchives) Drop(context.Context, string) (string, error)                { return f.drop, nil }
func (f *fakeArchives) PruneProcessedEvents(_ context.Context, ttl, batch int) (int64, error) {
	f.ttl, f.batch = ttl, batch
	return f.pruned, f.pruneErr
}

type nopStore struct{}

func (nopStore) PutArchive(context.Context, string, io.ReadSeeker, int64, time.Time) error {
	return nil
}
func (nopStore) ArchiveChecksum(context.Context, string, time.Time) (string, error) { return "", nil }
func (nopStore) Bucket() string                                                     { return "b" }

// archivalCtx is a Context wired for a partition step plus archival.
func archivalCtx(a *fakeArchives, log *levelLog) *Context {
	return &Context{
		Partitions: fakePartitions{res: []domain.PartitionResult{
			{Action: domain.PartitionCreated}, {Action: domain.PartitionExists},
			{Name: "audit_events_2027_01", Action: domain.PartitionSkippedDefaultHasRows},
		}},
		PrecreateMonths: 3, WritableTrailingMonths: 3, HotWindowDays: 90,
		Archives: a, ArchiveStore: nopStore{}, Logger: port.NewSlogStyleLogger(log, nil),
	}
}

type fakePartitions struct {
	res []domain.PartitionResult
	err error
}

func (f fakePartitions) EnsurePartitions(context.Context, int, int) ([]domain.PartitionResult, error) {
	return f.res, f.err
}

func TestReconcile_PartitionStep(t *testing.T) {
	res, err := Reconcile(context.Background(), archivalCtx(&fakeArchives{}, &levelLog{}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Attempted != 3 || res.Succeeded != 2 || res.Skipped != 1 || res.Failed != 0 {
		t.Errorf("result = %+v", res)
	}
}

// LLD §8.5: an old partition that clears the drop gate counts as a
// success; blocked or stalled ones fail the run (the CronJob surfaces it).
func TestReconcile_Archival(t *testing.T) {
	a := &fakeArchives{partitions: []string{"audit_events_2020_01", "audit_events_2099_01"}, drop: domain.DropDropped}
	log := &levelLog{}
	res, err := Reconcile(context.Background(), archivalCtx(a, log))
	if err != nil {
		t.Fatal(err)
	}
	if res.Attempted != 4 || res.Succeeded != 3 || res.Failed != 0 || log.infos == 0 {
		t.Errorf("result = %+v infos = %d", res, log.infos)
	}

	for drop, want := range map[string]string{domain.DropRedactionPending: "blocked", domain.DropNotVerified: "stalled"} {
		a := &fakeArchives{partitions: []string{"audit_events_2020_01"}, drop: drop}
		res, err := Reconcile(context.Background(), archivalCtx(a, &levelLog{}))
		if err == nil || !strings.Contains(err.Error(), "blocked or stalled") || res.Failed != 1 {
			t.Errorf("%s (%s): res = %+v err = %v", drop, want, res, err)
		}
	}

	boom := errors.New("db")
	if _, err := Reconcile(context.Background(), archivalCtx(&fakeArchives{listErr: boom}, &levelLog{})); !errors.Is(err, boom) {
		t.Errorf("archival error = %v", err)
	}
	unwired := archivalCtx(&fakeArchives{}, &levelLog{})
	unwired.Archives = nil
	if _, err := Reconcile(context.Background(), unwired); err == nil || !strings.Contains(err.Error(), "archival not wired") {
		t.Errorf("unwired archives: %v", err)
	}
	unwired = archivalCtx(&fakeArchives{}, &levelLog{})
	unwired.ArchiveStore = nil
	if _, err := Reconcile(context.Background(), unwired); err == nil {
		t.Error("unwired store must fail")
	}
}

// LLD §4.2: the ledger prune passes TTL and batch through and reports rows.
func TestProcessedEventsPrune(t *testing.T) {
	a := &fakeArchives{pruned: 42}
	log := &levelLog{}
	jctx := &Context{Ledger: a, ProcessedEventsTTLDays: 8, ProcessedEventsBatch: 10000, Logger: port.NewSlogStyleLogger(log, nil)}
	res, err := ProcessedEventsPrune(context.Background(), jctx)
	if err != nil || res.Attempted != 42 || res.Succeeded != 42 || a.ttl != 8 || a.batch != 10000 || log.infos != 1 {
		t.Errorf("res = %+v err = %v ttl=%d batch=%d", res, err, a.ttl, a.batch)
	}
	boom := errors.New("db")
	a.pruneErr, a.pruned = boom, 7
	res, err = ProcessedEventsPrune(context.Background(), jctx)
	if !errors.Is(err, boom) || res.Succeeded != 7 {
		t.Errorf("error path: res = %+v err = %v", res, err)
	}
	if _, err := ProcessedEventsPrune(context.Background(), &Context{}); err == nil {
		t.Error("missing ledger must fail")
	}
}

func TestReconcile_Errors(t *testing.T) {
	if _, err := Reconcile(context.Background(), &Context{}); err == nil {
		t.Error("missing PartitionManager must fail")
	}
	if _, err := Reconcile(context.Background(), &Context{Partitions: fakePartitions{}, PrecreateMonths: 99}); err == nil {
		t.Error("invalid window must fail")
	}
	boom := errors.New("db down")
	if _, err := Reconcile(context.Background(), &Context{Partitions: fakePartitions{err: boom}, PrecreateMonths: 3}); !errors.Is(err, boom) {
		t.Errorf("err = %v", err)
	}
}
