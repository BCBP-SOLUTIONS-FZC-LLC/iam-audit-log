package service

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
)

const (
	aTenantA = "aaaaaaaa-0000-4000-8000-000000000001"
	aTenantB = "bbbbbbbb-0000-4000-8000-000000000002"
	aPart    = "audit_events_2026_03"
)

var (
	aNow   = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	aMonth = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
)

// fakeArchRepo is an in-memory port.ArchiveRepository with a small state
// machine mirroring audit_event_archive_state.
type fakeArchRepo struct {
	partitions    []string
	partErr       error
	reopen        []string
	reopenListErr error
	reopenErr     error
	reopened      []string

	states      map[domain.RetentionTier]port.ArchiveTierState
	statesErr   error
	statesFrom  int // statesErr applies from this call (0 = always)
	stateCalls  int
	markFailErr error

	pending    bool
	pendingErr error

	rows       map[domain.RetentionTier][]domain.AuditEntry
	streamErr  error
	streamFail int // stream this many rows, then fail
	nextParts  map[string]int
	nextErr    error
	archErr    error

	upserts   []domain.ArchiveObject
	upsertErr error
	current   map[domain.RetentionTier][]domain.ArchiveObject
	finishes  []finishCall
	finishErr error

	unsealedOverride map[domain.RetentionTier][]domain.ArchiveObject
	unsealedErr      error
	verifyErr        error
	failed           map[domain.RetentionTier]string
	failCtxErr       error
	accessErr        error
	accessCalls      int
	resets           int
	resetErr         error

	drops   []string // results served in order; the last repeats
	dropErr error
	dropped int
	onDrop  func()
}

type finishCall struct {
	tier     domain.RetentionTier
	keep     []string
	rows     int64
	manifest string
	prefix   string
}

func newFakeArchRepo() *fakeArchRepo {
	return &fakeArchRepo{
		partitions: []string{aPart},
		states:     map[domain.RetentionTier]port.ArchiveTierState{},
		rows:       map[domain.RetentionTier][]domain.AuditEntry{},
		nextParts:  map[string]int{},
		current:    map[domain.RetentionTier][]domain.ArchiveObject{},
		failed:     map[domain.RetentionTier]string{},
		drops:      []string{domain.DropDropped},
	}
}

func (f *fakeArchRepo) Partitions(context.Context) ([]string, error) { return f.partitions, f.partErr }
func (f *fakeArchRepo) ReopenCandidates(context.Context) ([]string, error) {
	return f.reopen, f.reopenListErr
}

func (f *fakeArchRepo) Reopen(_ context.Context, p string) (int64, error) {
	if f.reopenErr != nil {
		return 0, f.reopenErr
	}
	f.reopened = append(f.reopened, p)
	return 3, nil
}

func (f *fakeArchRepo) States(context.Context, string) (map[domain.RetentionTier]port.ArchiveTierState, error) {
	f.stateCalls++
	out := map[domain.RetentionTier]port.ArchiveTierState{}
	for k, v := range f.states {
		out[k] = v
	}
	if f.stateCalls < f.statesFrom {
		return out, nil
	}
	return out, f.statesErr
}

func (f *fakeArchRepo) PendingRedaction(context.Context, string) (bool, error) {
	return f.pending, f.pendingErr
}

func (f *fakeArchRepo) MarkArchiving(_ context.Context, _ string, tier domain.RetentionTier, month time.Time) error {
	if f.archErr != nil {
		return f.archErr
	}
	if !month.Equal(aMonth) {
		return fmt.Errorf("month = %v", month)
	}
	f.states[tier] = port.ArchiveTierState{Status: domain.ArchiveArchiving}
	f.current[tier] = nil
	return nil
}

func (f *fakeArchRepo) NextParts(context.Context, string, domain.RetentionTier) (map[string]int, error) {
	out := map[string]int{}
	for k, v := range f.nextParts {
		out[k] = v
	}
	return out, f.nextErr
}

func (f *fakeArchRepo) StreamTier(_ context.Context, _ string, tier domain.RetentionTier, fn func(domain.AuditEntry) error) error {
	for i, e := range f.rows[tier] {
		if f.streamErr != nil && i == f.streamFail {
			return f.streamErr
		}
		if err := fn(e); err != nil {
			return err
		}
	}
	if f.streamErr != nil && f.streamFail >= len(f.rows[tier]) {
		return f.streamErr
	}
	return nil
}

func (f *fakeArchRepo) UpsertObject(_ context.Context, _ string, o domain.ArchiveObject) error {
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.upserts = append(f.upserts, o)
	f.current[o.Tier] = append(f.current[o.Tier], o)
	return nil
}

func (f *fakeArchRepo) FinishArchive(_ context.Context, _ string, tier domain.RetentionTier, keep []string, rows int64, manifest, prefix string) error {
	if f.finishErr != nil {
		return f.finishErr
	}
	f.finishes = append(f.finishes, finishCall{tier, keep, rows, manifest, prefix})
	f.states[tier] = port.ArchiveTierState{Status: domain.ArchiveArchived, SHA256Manifest: manifest}
	return nil
}

func (f *fakeArchRepo) UnsealedObjects(_ context.Context, _ string, tier domain.RetentionTier) ([]domain.ArchiveObject, error) {
	if o, ok := f.unsealedOverride[tier]; ok {
		return o, f.unsealedErr
	}
	return f.current[tier], f.unsealedErr
}

func (f *fakeArchRepo) MarkVerified(_ context.Context, _ string, tier domain.RetentionTier) error {
	if f.verifyErr != nil {
		return f.verifyErr
	}
	f.states[tier] = port.ArchiveTierState{Status: domain.ArchiveVerified, SHA256Manifest: f.states[tier].SHA256Manifest}
	return nil
}

func (f *fakeArchRepo) MarkFailed(ctx context.Context, _ string, tier domain.RetentionTier, reason string) error {
	f.failCtxErr = ctx.Err()
	if f.markFailErr != nil {
		return f.markFailErr
	}
	f.failed[tier] = reason
	f.states[tier] = port.ArchiveTierState{Status: domain.ArchiveFailed}
	return nil
}

func (f *fakeArchRepo) MarkAccessExpiring(_ context.Context, _ string, month time.Time) error {
	f.accessCalls++
	if !month.Equal(aMonth) {
		return fmt.Errorf("month = %v", month)
	}
	return f.accessErr
}

func (f *fakeArchRepo) ResetForRearchive(context.Context, string) error {
	f.resets++
	if f.resetErr != nil {
		return f.resetErr
	}
	for _, t := range domain.RetainedTiers {
		f.states[t] = port.ArchiveTierState{Status: domain.ArchivePending}
	}
	return nil
}

func (f *fakeArchRepo) Drop(context.Context, string) (string, error) {
	if f.onDrop != nil {
		f.onDrop()
	}
	if f.dropErr != nil {
		return "", f.dropErr
	}
	i := f.dropped
	if i >= len(f.drops) {
		i = len(f.drops) - 1
	}
	f.dropped++
	return f.drops[i], nil
}

// fakeArchStore keeps uploaded bodies; checksums are over the stored body.
type fakeArchStore struct {
	bodies   map[string][]byte
	retain   map[string]time.Time
	putErr   error
	onPut    func() error
	sumErr   error
	sumOver  map[string]string
	sumCalls int
}

func newFakeArchStore() *fakeArchStore {
	return &fakeArchStore{bodies: map[string][]byte{}, retain: map[string]time.Time{}}
}

func (s *fakeArchStore) Bucket() string { return "iam-audit-archive" }

func (s *fakeArchStore) PutArchive(_ context.Context, key string, body io.ReadSeeker, size int64, retainUntil time.Time) error {
	if s.putErr != nil {
		return s.putErr
	}
	if s.onPut != nil {
		if err := s.onPut(); err != nil {
			return err
		}
	}
	b, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if int64(len(b)) != size {
		return fmt.Errorf("size %d != body %d", size, len(b))
	}
	s.bodies[key], s.retain[key] = b, retainUntil
	return nil
}

func (s *fakeArchStore) ArchiveChecksum(_ context.Context, key string, _ time.Time) (string, error) {
	s.sumCalls++
	if s.sumErr != nil {
		return "", s.sumErr
	}
	if v, ok := s.sumOver[key]; ok {
		return v, nil
	}
	sum := sha256.Sum256(s.bodies[key])
	return hex.EncodeToString(sum[:]), nil
}

type fakeArchMetrics struct {
	archived map[string]int
	blocked  int
	pruned   map[domain.RetentionTier]int
	stalled  int
}

func newFakeArchMetrics() *fakeArchMetrics {
	return &fakeArchMetrics{archived: map[string]int{}, pruned: map[domain.RetentionTier]int{}, stalled: -1}
}

func (m *fakeArchMetrics) PartitionArchived(tier domain.RetentionTier, result string) {
	m.archived[string(tier)+"/"+result]++
}
func (m *fakeArchMetrics) RedactionBlocked()                { m.blocked++ }
func (m *fakeArchMetrics) Pruned(tier domain.RetentionTier) { m.pruned[tier]++ }
func (m *fakeArchMetrics) Stalled(n int)                    { m.stalled = n }

type archFixture struct {
	repo    *fakeArchRepo
	store   *fakeArchStore
	metrics *fakeArchMetrics
	log     *errLog
	dir     string
	svc     *ArchiveService
}

func newArchFixture(t *testing.T) *archFixture {
	t.Helper()
	fx := &archFixture{repo: newFakeArchRepo(), store: newFakeArchStore(), metrics: newFakeArchMetrics(), log: &errLog{}, dir: t.TempDir()}
	fx.svc = NewArchiveService(fx.repo, fx.store, fx.metrics, ArchiveConfig{
		HotWindowDays: 90, WritableTrailingMonths: 3, PartMaxRows: 2, WorkDir: fx.dir,
		Now: func() time.Time { return aNow },
	}, fx.log)
	return fx
}

// assertNoTempFiles checks every part temp file was removed.
func (fx *archFixture) assertNoTempFiles(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(fx.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("temp files left behind: %v", entries)
	}
}

func arow(tenant string, n int, tier domain.RetentionTier, actor string, target *domain.TargetRef) domain.AuditEntry {
	return domain.AuditEntry{
		ID: fmt.Sprintf("0190a1b2-0000-7000-8000-%012d", n), OccurredAt: aMonth.Add(time.Duration(n) * time.Hour),
		RecordedAt: aMonth.Add(time.Duration(n) * time.Hour), TenantID: tenant, EntryType: "user.updated", Action: "update",
		Actor: domain.ActorRef{Type: domain.ActorUser, ID: actor}, Target: target, SourceService: "svc",
		SourceEventType: "UserUpdated", SourceEventID: fmt.Sprintf("src-%d", n), RetentionTier: tier,
		IngestMode: domain.IngestBus, Metadata: json.RawMessage(`{"n":1}`),
	}
}

const (
	actorX = "cccccccc-0000-4000-8000-00000000000c"
	actorY = "dddddddd-0000-4000-8000-00000000000d"
	userZ  = "eeeeeeee-0000-4000-8000-00000000000e"
)

func seedHappy(r *fakeArchRepo) {
	r.rows[domain.TierSecurity3y] = []domain.AuditEntry{
		arow(aTenantA, 1, domain.TierSecurity3y, actorX, nil),
		arow(aTenantA, 2, domain.TierSecurity3y, actorY, &domain.TargetRef{Type: "user", ID: userZ}),
		arow(aTenantA, 3, domain.TierSecurity3y, actorX, nil),
		arow(aTenantB, 4, domain.TierSecurity3y, actorY, nil),
	}
	r.rows[domain.TierCompliance7y] = []domain.AuditEntry{
		arow(aTenantA, 5, domain.TierCompliance7y, actorX, nil),
	}
}

func gunzipRecords(t *testing.T, b []byte) []domain.ArchiveRecord {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	var out []domain.ArchiveRecord
	sc := bufio.NewScanner(zr)
	for sc.Scan() {
		var r domain.ArchiveRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// LLD §8.5/§15.4 happy path: per-tenant, size-split parts with D-10 keys;
// the manifest matches what was uploaded; both tiers verified; the drop
// gate is reached and every tier is pruned.
func TestArchive_HappyPath(t *testing.T) {
	fx := newArchFixture(t)
	seedHappy(fx.repo)
	sum, err := fx.svc.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Dropped != 1 || sum.Stalled != 0 || sum.Blocked != 0 || sum.Skipped != 0 {
		t.Fatalf("summary = %+v", sum)
	}

	wantKeys := []string{
		"security_3y/" + aTenantA + "/2026/03/audit_events_2026_03-part-0000.jsonl.gz",
		"security_3y/" + aTenantA + "/2026/03/audit_events_2026_03-part-0001.jsonl.gz",
		"security_3y/" + aTenantB + "/2026/03/audit_events_2026_03-part-0000.jsonl.gz",
		"compliance_7y/" + aTenantA + "/2026/03/audit_events_2026_03-part-0000.jsonl.gz",
	}
	if len(fx.repo.upserts) != len(wantKeys) {
		t.Fatalf("upserts = %d, want %d", len(fx.repo.upserts), len(wantKeys))
	}
	wantRows := []int{2, 1, 1, 1}
	for i, o := range fx.repo.upserts {
		if o.Key != wantKeys[i] {
			t.Errorf("part %d key = %s, want %s", i, o.Key, wantKeys[i])
		}
		body := fx.store.bodies[o.Key]
		recs := gunzipRecords(t, body)
		if len(recs) != wantRows[i] || o.RowCount != int64(wantRows[i]) {
			t.Errorf("part %d rows = %d/%d, want %d", i, len(recs), o.RowCount, wantRows[i])
		}
		sha := sha256.Sum256(body)
		if o.SHA256 != hex.EncodeToString(sha[:]) {
			t.Errorf("part %d sha mismatch", i)
		}
		if o.Bucket != "iam-audit-archive" || !o.PeriodMonth.Equal(aMonth) {
			t.Errorf("part %d bucket/month = %s/%v", i, o.Bucket, o.PeriodMonth)
		}
		for _, r := range recs {
			if r.TenantID != o.TenantID || r.RetentionTier != string(o.Tier) {
				t.Errorf("part %d holds a foreign row %+v", i, r)
			}
		}
		if !recs[0].OccurredAt.Equal(o.MinOccurredAt) || !recs[len(recs)-1].OccurredAt.Equal(o.MaxOccurredAt) {
			t.Errorf("part %d occurred bounds %v..%v vs %v..%v", i, o.MinOccurredAt, o.MaxOccurredAt, recs[0].OccurredAt, recs[len(recs)-1].OccurredAt)
		}
		if o.MinID != recs[0].ID || o.MaxID != recs[len(recs)-1].ID {
			t.Errorf("part %d id bounds %s..%s", i, o.MinID, o.MaxID)
		}
		var raw int64
		for _, r := range recs {
			b, _ := json.Marshal(r)
			raw += int64(len(b)) + 1
		}
		if o.ByteSize != raw {
			t.Errorf("part %d byte_size = %d, want %d", i, o.ByteSize, raw)
		}
		if want := domain.RetainUntil(o.Tier, o.MaxOccurredAt); !fx.store.retain[o.Key].Equal(want) {
			t.Errorf("part %d retain-until = %v, want %v", i, fx.store.retain[o.Key], want)
		}
	}
	// D-17: part 0 of tenant A holds actor X, actor Y and user target Z.
	if got := fx.repo.upserts[0].SubjectIDs; strings.Join(got, ",") != strings.Join([]string{actorX, actorY, userZ}, ",") {
		t.Errorf("subject ids = %v", got)
	}

	if len(fx.repo.finishes) != 2 {
		t.Fatalf("finishes = %+v", fx.repo.finishes)
	}
	sec := fx.repo.finishes[0]
	if sec.tier != domain.TierSecurity3y || sec.rows != 4 || len(sec.keep) != 3 || sec.prefix != "security_3y/" ||
		sec.manifest != domain.ManifestSHA256(fx.repo.upserts[:3]) {
		t.Errorf("security finish = %+v", sec)
	}
	if c := fx.repo.finishes[1]; c.tier != domain.TierCompliance7y || c.rows != 1 || c.prefix != "compliance_7y/" {
		t.Errorf("compliance finish = %+v", c)
	}
	for _, tier := range domain.RetainedTiers {
		if fx.repo.states[tier].Status != domain.ArchiveVerified {
			t.Errorf("%s state = %+v", tier, fx.repo.states[tier])
		}
		if fx.metrics.archived[string(tier)+"/verified"] != 1 || fx.metrics.pruned[tier] != 1 {
			t.Errorf("%s metrics: %+v / %+v", tier, fx.metrics.archived, fx.metrics.pruned)
		}
	}
	if fx.metrics.pruned[domain.TierAccess90d] != 1 || fx.metrics.stalled != 0 || fx.repo.accessCalls != 1 {
		t.Errorf("access pruned / stalled / access calls: %v %d %d", fx.metrics.pruned, fx.metrics.stalled, fx.repo.accessCalls)
	}
	if fx.store.sumCalls != 4 {
		t.Errorf("verify re-read %d objects, want 4", fx.store.sumCalls)
	}
	fx.assertNoTempFiles(t)
}

// D-20: a re-opened month appends parts after its sealed ones.
func TestArchive_NextPartsOffsetForReopenedMonth(t *testing.T) {
	fx := newArchFixture(t)
	seedHappy(fx.repo)
	fx.repo.nextParts = map[string]int{aTenantA: 5}
	if _, err := fx.svc.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range fx.repo.upserts {
		got = append(got, fmt.Sprintf("%s/%s/%d", o.Tier, o.TenantID[:8], o.Part))
	}
	want := []string{"security_3y/aaaaaaaa/5", "security_3y/aaaaaaaa/6", "security_3y/bbbbbbbb/0", "compliance_7y/aaaaaaaa/5"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("parts = %v, want %v", got, want)
	}
}

// A tier already verified is not re-archived; an empty tier finishes with
// no objects and still verifies.
func TestArchive_SkipsVerifiedTierAndHandlesEmptyTier(t *testing.T) {
	fx := newArchFixture(t)
	fx.repo.states[domain.TierSecurity3y] = port.ArchiveTierState{Status: domain.ArchiveVerified}
	sum, err := fx.svc.Run(context.Background())
	if err != nil || sum.Dropped != 1 {
		t.Fatalf("sum = %+v err = %v", sum, err)
	}
	if len(fx.repo.finishes) != 1 {
		t.Fatalf("finishes = %+v", fx.repo.finishes)
	}
	f := fx.repo.finishes[0]
	if f.tier != domain.TierCompliance7y || len(f.keep) != 0 || f.rows != 0 || f.manifest != domain.ManifestSHA256(nil) {
		t.Errorf("empty-tier finish = %+v", f)
	}
	if len(fx.store.bodies) != 0 {
		t.Errorf("nothing should be uploaded: %v", fx.store.bodies)
	}
}

// AL-D4: only months past the hot window and before the writable trailing
// months are archived; junk and DEFAULT names are skipped.
func TestArchive_SkipsIneligible(t *testing.T) {
	fx := newArchFixture(t)
	fx.repo.partitions = []string{"audit_events_2026_08", "audit_events_2026_07", "audit_events_default", "junk"}
	sum, err := fx.svc.Run(context.Background())
	if err != nil || sum.Skipped != 4 || sum.Dropped != 0 || fx.repo.dropped != 0 {
		t.Fatalf("sum = %+v err = %v drops = %d", sum, err, fx.repo.dropped)
	}
	if fx.metrics.stalled != 0 {
		t.Errorf("stalled = %d", fx.metrics.stalled)
	}
}

// AL-INV-12: a pending redaction task blocks archival outright.
func TestArchive_PendingRedactionBlocks(t *testing.T) {
	fx := newArchFixture(t)
	seedHappy(fx.repo)
	fx.repo.pending = true
	sum, err := fx.svc.Run(context.Background())
	if err != nil || sum.Blocked != 1 || sum.Dropped != 0 {
		t.Fatalf("sum = %+v err = %v", sum, err)
	}
	if fx.metrics.blocked != 1 || len(fx.repo.upserts) != 0 || fx.repo.dropped != 0 {
		t.Errorf("blocked=%d upserts=%d drops=%d", fx.metrics.blocked, len(fx.repo.upserts), fx.repo.dropped)
	}
	if fx.log.warns == 0 {
		t.Error("block must be logged")
	}
}

// AL-D4 late arrival: count_mismatch → reset → re-archive → dropped; a
// second mismatch in the same run defers the drop (stalled).
func TestArchive_CountMismatch(t *testing.T) {
	t.Run("re-archived once then dropped", func(t *testing.T) {
		fx := newArchFixture(t)
		seedHappy(fx.repo)
		fx.repo.drops = []string{domain.DropCountMismatch, domain.DropDropped}
		sum, err := fx.svc.Run(context.Background())
		if err != nil || sum.Dropped != 1 {
			t.Fatalf("sum = %+v err = %v", sum, err)
		}
		if fx.repo.resets != 1 || len(fx.repo.finishes) != 4 || fx.repo.dropped != 2 {
			t.Errorf("resets=%d finishes=%d drops=%d", fx.repo.resets, len(fx.repo.finishes), fx.repo.dropped)
		}
		fx.assertNoTempFiles(t)
	})
	t.Run("persistent late writes stall", func(t *testing.T) {
		fx := newArchFixture(t)
		seedHappy(fx.repo)
		fx.repo.drops = []string{domain.DropCountMismatch}
		sum, err := fx.svc.Run(context.Background())
		if err != nil || sum.Stalled != 1 || sum.Dropped != 0 {
			t.Fatalf("sum = %+v err = %v", sum, err)
		}
		if fx.repo.dropped != 2 || fx.metrics.stalled != 1 {
			t.Errorf("drops=%d stalled=%d", fx.repo.dropped, fx.metrics.stalled)
		}
	})
	t.Run("reset failure stalls", func(t *testing.T) {
		fx := newArchFixture(t)
		fx.repo.drops = []string{domain.DropCountMismatch}
		fx.repo.resetErr = errors.New("db")
		sum, err := fx.svc.Run(context.Background())
		if err != nil || sum.Stalled != 1 || fx.repo.dropped != 1 {
			t.Fatalf("sum = %+v err = %v drops = %d", sum, err, fx.repo.dropped)
		}
	})
}

// The drop gate's refusals: redaction_pending → blocked; not_verified →
// stalled (AL-INV-9 held); missing (dropped concurrently) → skipped, not a
// false stall alarm.
func TestArchive_DropRefusals(t *testing.T) {
	for res, want := range map[string]ArchiveSummary{
		domain.DropRedactionPending: {Blocked: 1},
		domain.DropNotVerified:      {Stalled: 1},
		domain.DropMissing:          {Skipped: 1},
	} {
		fx := newArchFixture(t)
		fx.repo.drops = []string{res}
		sum, err := fx.svc.Run(context.Background())
		if err != nil || sum.Blocked != want.Blocked || sum.Stalled != want.Stalled || sum.Skipped != want.Skipped || sum.Dropped != 0 {
			t.Errorf("%s: sum = %+v err = %v", res, sum, err)
		}
		if res == domain.DropRedactionPending && fx.metrics.blocked != 1 {
			t.Errorf("%s: blocked metric = %d", res, fx.metrics.blocked)
		}
	}
}

// Any failure keeps the partition (no drop), records the reason on a live
// context, counts a failed tier, and stalls.
func TestArchive_TierFailures(t *testing.T) {
	boom := errors.New("boom")
	cases := map[string]func(fx *archFixture){
		"mark archiving": func(fx *archFixture) { fx.repo.archErr = boom },
		"next parts":     func(fx *archFixture) { fx.repo.nextErr = boom },
		"upload":         func(fx *archFixture) { fx.store.putErr = boom },
		"stream":         func(fx *archFixture) { fx.repo.streamErr, fx.repo.streamFail = boom, 1 },
		"stream at end":  func(fx *archFixture) { fx.repo.streamErr, fx.repo.streamFail = boom, 99 },
		"upsert":         func(fx *archFixture) { fx.repo.upsertErr = boom },
		"finish":         func(fx *archFixture) { fx.repo.finishErr = boom },
		"verify read":    func(fx *archFixture) { fx.store.sumErr = boom },
		"unsealed list":  func(fx *archFixture) { fx.repo.unsealedErr = boom },
		"mark verified":  func(fx *archFixture) { fx.repo.verifyErr = boom },
		"verify states":  func(fx *archFixture) { fx.repo.statesErr, fx.repo.statesFrom = boom, 2 },
		"checksum mismatch": func(fx *archFixture) {
			fx.store.sumOver = map[string]string{
				"security_3y/" + aTenantA + "/2026/03/audit_events_2026_03-part-0000.jsonl.gz": "bad",
			}
		},
		"manifest mismatch": func(fx *archFixture) {
			fx.repo.unsealedOverride = make(map[domain.RetentionTier][]domain.ArchiveObject)
			fx.repo.unsealedOverride[domain.TierSecurity3y] = []domain.ArchiveObject{}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			fx := newArchFixture(t)
			seedHappy(fx.repo)
			setup(fx)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sum, err := fx.svc.Run(ctx)
			if err != nil || sum.Stalled != 1 || sum.Dropped != 0 {
				t.Fatalf("sum = %+v err = %v", sum, err)
			}
			if fx.repo.dropped != 0 {
				t.Error("a failed tier must never reach the drop gate (AL-INV-9)")
			}
			if fx.repo.failed[domain.TierSecurity3y] == "" || fx.metrics.archived["security_3y/failed"] != 1 {
				t.Errorf("failed = %v metrics = %v", fx.repo.failed, fx.metrics.archived)
			}
			if fx.repo.failCtxErr != nil {
				t.Error("MarkFailed must run on a live context")
			}
			if fx.metrics.stalled != 1 || fx.log.errs == 0 {
				t.Errorf("stalled=%d errs=%d", fx.metrics.stalled, fx.log.errs)
			}
			fx.assertNoTempFiles(t)
		})
	}
}

// MarkFailed still runs, on a fresh context, when the run's ctx is
// cancelled mid-upload (the reason must survive a shutdown).
func TestArchive_FailureRecordedAfterCancel(t *testing.T) {
	fx := newArchFixture(t)
	seedHappy(fx.repo)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fx.store.onPut = func() error { cancel(); return context.Canceled }
	sum, err := fx.svc.Run(ctx)
	if err != nil || sum.Stalled != 1 {
		t.Fatalf("sum = %+v err = %v", sum, err)
	}
	if fx.repo.failed[domain.TierSecurity3y] == "" || fx.repo.failCtxErr != nil {
		t.Errorf("failed = %v failCtxErr = %v", fx.repo.failed, fx.repo.failCtxErr)
	}
	fx.assertNoTempFiles(t)
}

// Other per-partition step failures stall without an error from Run.
func TestArchive_StepFailuresStall(t *testing.T) {
	boom := errors.New("boom")
	for name, setup := range map[string]func(*fakeArchRepo){
		"pending check": func(r *fakeArchRepo) { r.pendingErr = boom },
		"states":        func(r *fakeArchRepo) { r.statesErr = boom },
		"access":        func(r *fakeArchRepo) { r.accessErr = boom },
		"drop":          func(r *fakeArchRepo) { r.dropErr = boom },
	} {
		fx := newArchFixture(t)
		setup(fx.repo)
		sum, err := fx.svc.Run(context.Background())
		if err != nil || sum.Stalled != 1 || sum.Dropped != 0 {
			t.Errorf("%s: sum = %+v err = %v", name, sum, err)
		}
	}
}

// D-19: dropped months with late DEFAULT rows are re-opened first; list or
// re-open failures abort the pass.
func TestArchive_ReopenAndRunErrors(t *testing.T) {
	fx := newArchFixture(t)
	fx.repo.reopen = []string{"audit_events_2025_12"}
	fx.repo.partitions = nil
	sum, err := fx.svc.Run(context.Background())
	if err != nil || sum.Reopened != 1 || fmt.Sprint(fx.repo.reopened) != "[audit_events_2025_12]" || fx.log.warns == 0 {
		t.Fatalf("sum = %+v err = %v reopened = %v", sum, err, fx.repo.reopened)
	}

	boom := errors.New("db")
	for name, setup := range map[string]func(*fakeArchRepo){
		"list reopen": func(r *fakeArchRepo) { r.reopenListErr = boom },
		"reopen":      func(r *fakeArchRepo) { r.reopen, r.reopenErr = []string{"audit_events_2025_12"}, boom },
		"partitions":  func(r *fakeArchRepo) { r.partErr = boom },
	} {
		fx := newArchFixture(t)
		setup(fx.repo)
		if _, err := fx.svc.Run(context.Background()); !errors.Is(err, boom) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// A cancelled context stops the pass before the next partition.
func TestArchive_ContextCancelled(t *testing.T) {
	fx := newArchFixture(t)
	fx.repo.partitions = []string{aPart, "audit_events_2026_02"}
	ctx, cancel := context.WithCancel(context.Background())
	fx.repo.onDrop = cancel
	sum, err := fx.svc.Run(ctx)
	if !errors.Is(err, context.Canceled) || sum.Dropped != 1 {
		t.Fatalf("sum = %+v err = %v", sum, err)
	}
}

// Defaults and nil collaborators are tolerated.
func TestNewArchiveService_Defaults(t *testing.T) {
	svc := NewArchiveService(newFakeArchRepo(), newFakeArchStore(), nil, ArchiveConfig{WorkDir: t.TempDir()}, nil)
	if svc.cfg.PartMaxRows != 50000 || svc.cfg.Now == nil {
		t.Errorf("cfg = %+v", svc.cfg)
	}
	// Runs end to end with no metrics or logger (real clock: 2026_03 is due).
	svc.repo.(*fakeArchRepo).drops = []string{domain.DropRedactionPending}
	if _, err := svc.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	svc.repo.(*fakeArchRepo).drops = []string{domain.DropCountMismatch}
	svc.repo.(*fakeArchRepo).pending = false
	if _, err := svc.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := newFakeArchRepo()
	r.archErr = errors.New("x")
	if _, err := NewArchiveService(r, newFakeArchStore(), nil, ArchiveConfig{}, nil).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// A part temp file that cannot be created fails the tier cleanly.
func TestArchive_TempDirUnavailable(t *testing.T) {
	fx := newArchFixture(t)
	seedHappy(fx.repo)
	fx.svc.cfg.WorkDir = fx.dir + "/does-not-exist"
	sum, err := fx.svc.Run(context.Background())
	if err != nil || sum.Stalled != 1 || len(fx.store.bodies) != 0 {
		t.Fatalf("sum = %+v err = %v", sum, err)
	}
	if !strings.Contains(fx.repo.failed[domain.TierSecurity3y], "temp file") {
		t.Errorf("reason = %q", fx.repo.failed[domain.TierSecurity3y])
	}
}

// A MarkFailed that itself fails is logged; the partition still stalls.
func TestArchive_MarkFailedErrorLogged(t *testing.T) {
	fx := newArchFixture(t)
	seedHappy(fx.repo)
	fx.store.putErr = errors.New("s3")
	fx.repo.markFailErr = errors.New("db")
	sum, err := fx.svc.Run(context.Background())
	if err != nil || sum.Stalled != 1 || fx.log.errs < 3 {
		t.Fatalf("sum = %+v err = %v errs = %d", sum, err, fx.log.errs)
	}
}
