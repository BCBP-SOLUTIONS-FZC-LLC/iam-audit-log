package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
)

type fakePlans struct {
	calls        int
	tenant, plan string
	days         int
	at           time.Time
	err          error
}

func (f *fakePlans) ProjectPlan(_ context.Context, tenant, plan string, days int, at time.Time) (bool, error) {
	f.calls++
	f.tenant, f.plan, f.days, f.at = tenant, plan, days, at
	return true, f.err
}

type fakeWindows map[string]int

func (w fakeWindows) WindowDays(p string) (int, bool) { d, ok := w[p]; return d, ok }

func busEv(topic, typ, payload string) domain.BusEvent {
	return domain.BusEvent{ID: "evt-" + typ, Type: typ, Source: "p", Topic: topic,
		TenantID: "11111111-1111-1111-1111-111111111111", Actor: "iam-system",
		Time: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), Payload: json.RawMessage(payload)}
}

// AL-INV-2: the bus path persists through the SAME Append as direct-write,
// with the queue's consumer discriminator; a redelivery is a replay.
func TestIngestBus_SameAppendPathAndReplay_ALINV2_ALINV4(t *testing.T) {
	store := &fakeStore{}
	svc := NewIngestService(store, seqIDs(), 8192, 500, &recLog{}).WithBus(BusIngestConfig{})
	res, err := svc.IngestBus(context.Background(), busEv(domain.TopicUser, "UserUpdated", `{"user_id":"u"}`), "user")
	if err != nil || !res.Created || !res.Known {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if store.consumer != "user" || res.Entry.IngestMode != domain.IngestBus {
		t.Errorf("consumer=%s mode=%s", store.consumer, res.Entry.IngestMode)
	}
	again, err := svc.IngestBus(context.Background(), busEv(domain.TopicUser, "UserUpdated", `{"user_id":"u"}`), "user")
	if err != nil || again.Created || again.Entry.ID != res.Entry.ID {
		t.Fatalf("redelivery must be a no-op replay: %+v %v", again, err)
	}
}

// AL-EVT-4: an unknown type is persisted (Known=false) and warned.
func TestIngestBus_UnknownPersistedAndWarned_ALEVT4(t *testing.T) {
	store, log := &fakeStore{}, &recLog{}
	svc := NewIngestService(store, seqIDs(), 8192, 500, log).WithBus(BusIngestConfig{})
	res, err := svc.IngestBus(context.Background(), busEv(domain.TopicTender, "TenderShredded", `{}`), "tender")
	if err != nil || res.Known || res.Entry.EntryType != "tender.unknown" || len(store.appended) != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if log.warns != 1 {
		t.Errorf("warns = %d", log.warns)
	}
}

// §4.2: plan-carrying tenant events feed tenant_plan_window with the event
// time (recency guard input) and the CAT-I2 window when known, else the
// conservative default.
func TestIngestBus_PlanProjection(t *testing.T) {
	plans := &fakePlans{}
	svc := NewIngestService(&fakeStore{}, seqIDs(), 8192, 500, nil).WithBus(BusIngestConfig{
		Plans: plans, Windows: fakeWindows{"enterprise": 2555}, DefaultWindowDays: 365,
	})
	ctx := context.Background()
	if _, err := svc.IngestBus(ctx, busEv(domain.TopicTenant, "TenantCreated", `{"plan":"enterprise"}`), "tenant"); err != nil {
		t.Fatal(err)
	}
	if plans.calls != 1 || plans.plan != "enterprise" || plans.days != 2555 || !plans.at.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("projection = %+v", plans)
	}
	if _, err := svc.IngestBus(ctx, busEv(domain.TopicBilling, "TenantPlanChanged", `{"plan":"starter"}`), "billing"); err != nil {
		t.Fatal(err)
	}
	if plans.days != 365 {
		t.Errorf("unknown plan → default window, got %d", plans.days)
	}
	for _, e := range []domain.BusEvent{
		busEv(domain.TopicTenant, "DirectPaidSignup", `{"direct_signup":true}`), // no plan field
		busEv(domain.TopicBilling, "TenantPlanChanged", `{}`),                   // plan optional
		busEv(domain.TopicUser, "UserUpdated", `{"plan":"pro"}`),                // not a plan event
		busEv(domain.TopicTenant, "TrialStarted", `{"plan":"Bad Plan!"}`),       // malformed code
	} {
		before := plans.calls
		if _, err := svc.IngestBus(ctx, e, "x"); err != nil {
			t.Fatal(err)
		}
		if plans.calls != before {
			t.Errorf("%s must not project", e.Type)
		}
	}
}

func TestIngestBus_Errors(t *testing.T) {
	ctx := context.Background()
	if _, err := NewIngestService(&fakeStore{}, seqIDs(), 8192, 500, nil).IngestBus(ctx, busEv(domain.TopicUser, "UserUpdated", `{}`), "user"); err == nil {
		t.Error("bus path must be configured")
	}
	svc := NewIngestService(&fakeStore{}, seqIDs(), 8192, 500, nil).WithBus(BusIngestConfig{})
	bad := busEv(domain.TopicUser, "UserUpdated", `{}`)
	bad.TenantID = "system"
	if _, err := svc.IngestBus(ctx, bad, "user"); err == nil {
		t.Error("an unstorable tenant must fail (→ redelivery → DLQ)")
	}
	noID := NewIngestService(&fakeStore{}, func() (string, error) { return "", errors.New("x") }, 8192, 500, nil).WithBus(BusIngestConfig{})
	if _, err := noID.IngestBus(ctx, busEv(domain.TopicUser, "UserUpdated", `{}`), "user"); err == nil {
		t.Error("id failure must surface")
	}
	down := NewIngestService(&fakeStore{err: errors.New("db")}, seqIDs(), 8192, 500, nil).WithBus(BusIngestConfig{})
	if _, err := down.IngestBus(ctx, busEv(domain.TopicUser, "UserUpdated", `{}`), "user"); err == nil {
		t.Error("store failure must surface (→ redelivery)")
	}
	planFail := NewIngestService(&fakeStore{}, seqIDs(), 8192, 500, nil).WithBus(BusIngestConfig{Plans: &fakePlans{err: errors.New("db")}})
	if _, err := planFail.IngestBus(ctx, busEv(domain.TopicTenant, "TenantCreated", `{"plan":"pro"}`), "tenant"); err == nil {
		t.Error("projection failure must surface so the redelivery heals it")
	}
}
