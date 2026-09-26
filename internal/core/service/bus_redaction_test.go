package service

import (
	"context"
	"errors"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
)

const redactSubject = "7f3c2d1e-9a8b-4c5d-8e7f-0a1b2c3d4e5f"

// fakeRedactionStore records AppendWithRedaction/ApplyRedaction calls.
type fakeRedactionStore struct {
	fakeStore
	req         *domain.RedactionRequest
	taskCreated bool
	appendErr   error
	outcome     domain.RedactionOutcome
	applyErr    error
	applied     []string
}

func (f *fakeRedactionStore) AppendWithRedaction(ctx context.Context, e domain.AuditEntry, consumer string, req domain.RedactionRequest) (domain.AuditEntry, bool, bool, error) {
	f.req = &req
	if f.appendErr != nil {
		return domain.AuditEntry{}, false, false, f.appendErr
	}
	stored, created, err := f.Append(ctx, e, consumer)
	return stored, created, f.taskCreated, err
}

func (f *fakeRedactionStore) ApplyRedaction(_ context.Context, id string) (domain.RedactionOutcome, error) {
	f.applied = append(f.applied, id)
	return f.outcome, f.applyErr
}

type statusCounts map[string]int

func (m statusCounts) TaskOutcome(s string) { m[s]++ }

type errLog struct{ infos, warns, errs int }

func (l *errLog) Debug(string, map[string]any) {}
func (l *errLog) Info(string, map[string]any)  { l.infos++ }
func (l *errLog) Warn(string, map[string]any)  { l.warns++ }
func (l *errLog) Error(string, map[string]any) { l.errs++ }

// redactionSvc wires a bus ingest service whose plain Append store and
// RedactionStore are separate fakes, so tests can tell which path ran.
func redactionSvc(rs *fakeRedactionStore, m statusCounts, log *errLog, ids func() (string, error)) (*IngestService, *fakeStore) {
	plain := &fakeStore{}
	cfg := BusIngestConfig{Redaction: rs}
	if m != nil {
		cfg.RedactionMetrics = m
	}
	if log == nil { // avoid a typed-nil port.Logger
		return NewIngestService(plain, ids, 8192, 500, nil).WithBus(cfg), plain
	}
	return NewIngestService(plain, ids, 8192, 500, log).WithBus(cfg), plain
}

func userDeleted(payload string) domain.BusEvent {
	return busEv(domain.TopicUser, "UserDeleted", payload)
}

// LLD §8.7 / AL-INV-12: UserDeleted schedules its task in the ingest
// transaction and applies it right after commit.
func TestIngestBus_UserDeletedSchedulesAndAppliesRedaction_ALINV12(t *testing.T) {
	for _, tc := range []struct {
		status string
		errs   int
	}{
		{domain.RedactionApplied, 0}, {domain.RedactionNotApplicable, 0}, {domain.RedactionMissed, 0}, // missed is routine (AL-Q15 Option A)
	} {
		rs := &fakeRedactionStore{taskCreated: true, outcome: domain.RedactionOutcome{Status: tc.status, RowsRedacted: 2}}
		m, log := statusCounts{}, &errLog{}
		svc, plain := redactionSvc(rs, m, log, seqIDs())
		ev := userDeleted(`{"user_id":"` + redactSubject + `"}`)
		res, err := svc.IngestBus(context.Background(), ev, "user")
		if err != nil || !res.Created || res.Entry.EntryType != "user.deleted" {
			t.Fatalf("%s: res=%+v err=%v", tc.status, res, err)
		}
		if len(plain.appended) != 0 {
			t.Errorf("%s: the trigger must use AppendWithRedaction, not Append", tc.status)
		}
		want := domain.RedactionRequest{
			TaskID: rs.req.TaskID, TenantID: ev.TenantID, SubjectID: redactSubject,
			TriggerEventType: "UserDeleted", TriggerSourceEventID: ev.ID,
		}
		if *rs.req != want || rs.req.TaskID == "" || rs.req.TaskID == res.Entry.ID {
			t.Errorf("%s: request = %+v (entry id %s)", tc.status, *rs.req, res.Entry.ID)
		}
		if len(rs.applied) != 1 || rs.applied[0] != rs.req.TaskID {
			t.Errorf("%s: applied = %v, want [%s]", tc.status, rs.applied, rs.req.TaskID)
		}
		if m[tc.status] != 1 || len(m) != 1 {
			t.Errorf("%s: metrics = %v", tc.status, m)
		}
		if log.errs != tc.errs {
			t.Errorf("%s: error logs = %d, want %d", tc.status, log.errs, tc.errs)
		}
	}
}

// A redelivered trigger (task already exists) is not re-applied inline;
// redaction-retry owns stuck tasks (gap 35).
func TestIngestBus_RedeliveredTriggerNotReapplied(t *testing.T) {
	rs := &fakeRedactionStore{taskCreated: false}
	m := statusCounts{}
	svc, _ := redactionSvc(rs, m, &errLog{}, seqIDs())
	if _, err := svc.IngestBus(context.Background(), userDeleted(`{"user_id":"`+redactSubject+`"}`), "user"); err != nil {
		t.Fatal(err)
	}
	if len(rs.applied) != 0 || len(m) != 0 {
		t.Errorf("applied=%v metrics=%v", rs.applied, m)
	}
}

// A failed immediate apply never fails the message: the task stays pending.
func TestIngestBus_ApplyFailureLeavesPending(t *testing.T) {
	rs := &fakeRedactionStore{taskCreated: true, applyErr: errors.New("db")}
	m, log := statusCounts{}, &errLog{}
	svc, _ := redactionSvc(rs, m, log, seqIDs())
	res, err := svc.IngestBus(context.Background(), userDeleted(`{"user_id":"`+redactSubject+`"}`), "user")
	if err != nil || !res.Created {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if m[domain.RedactionPending] != 1 || log.errs != 1 {
		t.Errorf("metrics=%v errors=%d", m, log.errs)
	}
	// Nil logger/metrics are tolerated.
	rs2 := &fakeRedactionStore{taskCreated: true, applyErr: errors.New("db")}
	svc2, _ := redactionSvc(rs2, nil, nil, seqIDs())
	if _, err := svc2.IngestBus(context.Background(), userDeleted(`{"user_id":"`+redactSubject+`"}`), "user"); err != nil {
		t.Fatal(err)
	}
}

func TestIngestBus_RedactionErrors(t *testing.T) {
	boom := errors.New("db")
	rs := &fakeRedactionStore{appendErr: boom}
	svc, _ := redactionSvc(rs, statusCounts{}, &errLog{}, seqIDs())
	if _, err := svc.IngestBus(context.Background(), userDeleted(`{"user_id":"`+redactSubject+`"}`), "user"); !errors.Is(err, boom) {
		t.Errorf("append error: %v", err)
	}

	// The entry id mints fine; the task id mint fails.
	calls := 0
	ids := func() (string, error) {
		calls++
		if calls == 2 {
			return "", errors.New("entropy")
		}
		return "0190a1b2-0000-7000-8000-000000000001", nil
	}
	svc, _ = redactionSvc(&fakeRedactionStore{taskCreated: true}, nil, nil, ids)
	if _, err := svc.IngestBus(context.Background(), userDeleted(`{"user_id":"`+redactSubject+`"}`), "user"); err == nil {
		t.Error("task id failure must surface")
	}
}

// Gap 36: a UserDeleted with no usable user_id is still audited (plain
// Append) and logged at error; no task is attempted.
func TestIngestBus_UserDeletedWithoutSubject(t *testing.T) {
	for _, payload := range []string{`{}`, `{"user_id":"not-a-uuid"}`} {
		rs := &fakeRedactionStore{taskCreated: true}
		log := &errLog{}
		svc, plain := redactionSvc(rs, statusCounts{}, log, seqIDs())
		if _, err := svc.IngestBus(context.Background(), userDeleted(payload), "user"); err != nil {
			t.Fatal(err)
		}
		if len(plain.appended) != 1 || rs.req != nil || len(rs.applied) != 0 || log.errs != 1 {
			t.Errorf("%s: plain=%d req=%v applied=%v errs=%d", payload, len(plain.appended), rs.req, rs.applied, log.errs)
		}
		// Nil logger tolerated.
		svcNoLog, _ := redactionSvc(&fakeRedactionStore{}, nil, nil, seqIDs())
		if _, err := svcNoLog.IngestBus(context.Background(), userDeleted(payload), "user"); err != nil {
			t.Fatal(err)
		}
	}
}

// Non-trigger events, and a service without Redaction, use plain Append.
func TestIngestBus_NonTriggerAndDisabledUsePlainAppend(t *testing.T) {
	rs := &fakeRedactionStore{taskCreated: true}
	svc, plain := redactionSvc(rs, statusCounts{}, &errLog{}, seqIDs())
	for _, ev := range []domain.BusEvent{
		busEv(domain.TopicUser, "UserUpdated", `{"user_id":"`+redactSubject+`"}`),
		busEv(domain.TopicTenant, "TenantOffboarded", `{}`), // D-16: no task
	} {
		if _, err := svc.IngestBus(context.Background(), ev, "x"); err != nil {
			t.Fatal(err)
		}
	}
	if len(plain.appended) != 2 || rs.req != nil {
		t.Errorf("plain=%d req=%v", len(plain.appended), rs.req)
	}

	off := &fakeStore{}
	disabled := NewIngestService(off, seqIDs(), 8192, 500, nil).WithBus(BusIngestConfig{})
	if _, err := disabled.IngestBus(context.Background(), userDeleted(`{"user_id":"`+redactSubject+`"}`), "user"); err != nil || len(off.appended) != 1 {
		t.Errorf("disabled: appended=%d err=%v", len(off.appended), err)
	}
}
