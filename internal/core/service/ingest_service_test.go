package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
)

type fakeStore struct {
	byKey    map[string]domain.AuditEntry
	appended []domain.AuditEntry
	consumer string
	err      error
}

func (f *fakeStore) Append(_ context.Context, e domain.AuditEntry, consumer string) (domain.AuditEntry, bool, error) {
	f.consumer = consumer
	if f.err != nil {
		return domain.AuditEntry{}, false, f.err
	}
	if f.byKey == nil {
		f.byKey = map[string]domain.AuditEntry{}
	}
	if prev, ok := f.byKey[e.SourceEventID]; ok {
		return prev, false, nil
	}
	e.RecordedAt = time.Now()
	f.byKey[e.SourceEventID] = e
	f.appended = append(f.appended, e)
	return e, true, nil
}

func seqIDs() func() (string, error) {
	n := 0
	return func() (string, error) {
		n++
		return "0190a1b2-0000-7000-8000-00000000000" + string(rune('0'+n)), nil
	}
}

func cmd(key string) domain.DirectWriteCommand {
	return domain.DirectWriteCommand{
		IdempotencyKey: key, TenantID: "11111111-1111-1111-1111-111111111111",
		EntryType: "config.idp.changed", Action: "update",
		Actor:      domain.ActorRef{Type: domain.ActorIAMSystem, ID: domain.IAMSystemActorID},
		OccurredAt: time.Now(), SourceService: "iam-realm-provisioner", SourceEventType: "TenantIdpConfigChanged",
		Metadata: json.RawMessage(`{"alias":"okta"}`),
	}
}

// AL-INV-2/4: every entry goes through the one Append with consumer
// "direct_write"; a replayed key returns the persisted entry, created=false.
func TestDirectWrite_CreatedThenReplay_ALINV4(t *testing.T) {
	store := &fakeStore{}
	svc := NewIngestService(store, seqIDs(), 8192, 500, nil)
	first, created, err := svc.DirectWrite(context.Background(), cmd("k1"))
	if err != nil || !created {
		t.Fatalf("first: created=%v err=%v", created, err)
	}
	if store.consumer != domain.ConsumerDirectWrite {
		t.Errorf("consumer = %s", store.consumer)
	}
	second, created, err := svc.DirectWrite(context.Background(), cmd("k1"))
	if err != nil || created || second.ID != first.ID {
		t.Fatalf("replay: created=%v id=%s/%s err=%v", created, second.ID, first.ID, err)
	}
	if len(store.appended) != 1 {
		t.Errorf("replay must not persist a second row")
	}
	if first.RetentionTier != domain.TierSecurity3y || first.ID == "" {
		t.Errorf("entry = %+v", first)
	}
}

func TestDirectWrite_ValidationAndErrors(t *testing.T) {
	store := &fakeStore{}
	svc := NewIngestService(store, seqIDs(), 8192, 500, &recLog{})
	bad := cmd("k")
	bad.EntryType = "auth.login.success" // bus type (D-6)
	if _, _, err := svc.DirectWrite(context.Background(), bad); err == nil || len(store.appended) != 0 {
		t.Fatal("rejected entries must never reach the store")
	}

	failID := NewIngestService(store, func() (string, error) { return "", errors.New("entropy") }, 8192, 500, nil)
	if _, _, err := failID.DirectWrite(context.Background(), cmd("k")); err == nil {
		t.Fatal("id minting failure must surface")
	}

	down := domain.NewError(domain.ErrDependencyUnavailable, "db down")
	failStore := NewIngestService(&fakeStore{err: down}, seqIDs(), 8192, 500, nil)
	if _, _, err := failStore.DirectWrite(context.Background(), cmd("k")); !errors.Is(err, down) {
		t.Fatalf("err = %v", err)
	}
}

// AL-6 partial success (LLD §5.4): per-entry results; one bad entry never
// rejects the batch; batch size and emptiness are whole-batch errors.
func TestDirectWriteBatch_PartialSuccess(t *testing.T) {
	store := &fakeStore{}
	svc := NewIngestService(store, seqIDs(), 8192, 3, nil)
	bad := cmd("b")
	bad.Actor = domain.ActorRef{Type: domain.ActorUser} // invalid_actor
	res, err := svc.DirectWriteBatch(context.Background(), []domain.DirectWriteCommand{cmd("a"), bad, cmd("a")})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Err != nil || !res[0].Created {
		t.Errorf("[0] = %+v", res[0])
	}
	var de *domain.Error
	if !errors.As(res[1].Err, &de) || de.Code != domain.ErrInvalidActor {
		t.Errorf("[1] = %+v", res[1])
	}
	if res[2].Err != nil || res[2].Created || res[2].Entry.ID != res[0].Entry.ID {
		t.Errorf("[2] must be a replay of [0]: %+v", res[2])
	}

	if _, err := svc.DirectWriteBatch(context.Background(), nil); !errors.As(err, &de) || de.Code != domain.ErrInvalidRequest {
		t.Errorf("empty: %v", err)
	}
	four := []domain.DirectWriteCommand{cmd("1"), cmd("2"), cmd("3"), cmd("4")}
	if _, err := svc.DirectWriteBatch(context.Background(), four); !errors.As(err, &de) || de.Code != domain.ErrBatchTooLarge {
		t.Errorf("too large: %v", err)
	}
	if svc.MaxBatch() != 3 {
		t.Error("MaxBatch")
	}
}

// A database outage mid-batch stops hammering it: remaining entries report
// the same dependency error.
func TestDirectWriteBatch_StopsOnDependencyOutage(t *testing.T) {
	down := domain.NewError(domain.ErrDependencyUnavailable, "db down")
	store := &fakeStore{err: down}
	svc := NewIngestService(store, seqIDs(), 8192, 10, nil)
	res, err := svc.DirectWriteBatch(context.Background(), []domain.DirectWriteCommand{cmd("1"), cmd("2"), cmd("3")})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if !errors.Is(r.Err, down) {
			t.Errorf("[%d] = %v", r.Index, r.Err)
		}
	}
}
