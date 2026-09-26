package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
)

type fakeRedactions struct {
	ids       []string
	listErr   error
	outcomes  map[string]domain.RedactionOutcome
	errs      map[string]error
	olderThan time.Time
	limit     int
	applied   []string
	onApply   func()
	swept     int64
	sweepErr  error
	window    time.Duration
}

func (f *fakeRedactions) Sweep(_ context.Context, window time.Duration) (int64, error) {
	f.window = window
	return f.swept, f.sweepErr
}

func (f *fakeRedactions) PendingTasks(_ context.Context, olderThan time.Time, limit int) ([]string, error) {
	f.olderThan, f.limit = olderThan, limit
	return f.ids, f.listErr
}

func (f *fakeRedactions) ApplyRedaction(_ context.Context, id string) (domain.RedactionOutcome, error) {
	f.applied = append(f.applied, id)
	if f.onApply != nil {
		f.onApply()
	}
	return f.outcomes[id], f.errs[id]
}

type countMetrics map[string]int

func (m countMetrics) TaskOutcome(status string) { m[status]++ }

type levelLog struct{ infos, warns, errs int }

func (l *levelLog) Debug(string, map[string]any) {}
func (l *levelLog) Info(string, map[string]any)  { l.infos++ }
func (l *levelLog) Warn(string, map[string]any)  { l.warns++ }
func (l *levelLog) Error(string, map[string]any) { l.errs++ }

func retryCtx(r *fakeRedactions, m countMetrics, log *levelLog) *Context {
	c := &Context{Redactions: r, RedactionRetryMinAge: 5 * time.Minute, RedactionRetryBatch: 7,
		Logger: port.NewSlogStyleLogger(log)}
	if m != nil {
		c.RedactionMetrics = m
	}
	return c
}

// RB-7: stuck tasks are re-applied; each outcome is counted, a failure
// stays pending (and fails the run) and is logged at error; missed is a
// routine outcome (AL-Q15 Option A) logged at info.
func TestRedactionRetry_MixedOutcomes(t *testing.T) {
	r := &fakeRedactions{
		ids: []string{"a", "b", "c", "d"},
		outcomes: map[string]domain.RedactionOutcome{
			"a": {Status: domain.RedactionApplied, RowsRedacted: 3},
			"b": {Status: domain.RedactionNotApplicable},
			"c": {Status: domain.RedactionMissed, RowsRedacted: 1},
		},
		errs: map[string]error{"d": errors.New("db")},
	}
	m, log := countMetrics{}, &levelLog{}
	before := time.Now()
	res, err := RedactionRetry(context.Background(), retryCtx(r, m, log))
	if err == nil {
		t.Fatal("a still-failing task must fail the run")
	}
	if res.Attempted != 4 || res.Succeeded != 3 || res.Failed != 1 {
		t.Errorf("result = %+v", res)
	}
	want := countMetrics{domain.RedactionApplied: 1, domain.RedactionNotApplicable: 1, domain.RedactionMissed: 1, domain.RedactionPending: 1}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("metric %s = %d, want %d (all: %v)", k, m[k], v, m)
		}
	}
	if log.errs != 1 || log.infos != 3 {
		t.Errorf("logs: errors=%d infos=%d, want 1/3 (only the failure at error; missed is info)", log.errs, log.infos)
	}
	if r.limit != 7 {
		t.Errorf("batch limit = %d", r.limit)
	}
	if d := before.Add(-5 * time.Minute).Sub(r.olderThan); d < -time.Second || d > time.Second {
		t.Errorf("olderThan = %v, want ≈ now-5m", r.olderThan)
	}
}

func TestRedactionRetry_EmptyAndNoMetrics(t *testing.T) {
	res, err := RedactionRetry(context.Background(), retryCtx(&fakeRedactions{}, nil, &levelLog{}))
	if err != nil || res != (Result{}) {
		t.Errorf("empty: %+v %v", res, err)
	}
	r := &fakeRedactions{ids: []string{"a"}, outcomes: map[string]domain.RedactionOutcome{"a": {Status: domain.RedactionApplied}}}
	if res, err := RedactionRetry(context.Background(), retryCtx(r, nil, &levelLog{})); err != nil || res.Succeeded != 1 {
		t.Errorf("nil metrics: %+v %v", res, err)
	}
}

func TestRedactionRetry_Errors(t *testing.T) {
	if _, err := RedactionRetry(context.Background(), &Context{}); err == nil {
		t.Error("missing RedactionTasks must fail")
	}
	boom := errors.New("db down")
	if _, err := RedactionRetry(context.Background(), retryCtx(&fakeRedactions{listErr: boom}, nil, &levelLog{})); !errors.Is(err, boom) {
		t.Errorf("list error: %v", err)
	}
}

func TestRedactionRetry_StopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &fakeRedactions{ids: []string{"a", "b", "c"}, onApply: cancel,
		outcomes: map[string]domain.RedactionOutcome{"a": {Status: domain.RedactionApplied}}}
	res, err := RedactionRetry(ctx, retryCtx(r, countMetrics{}, &levelLog{}))
	if !errors.Is(err, context.Canceled) || len(r.applied) != 1 || res.Attempted != 1 {
		t.Errorf("cancel: res=%+v err=%v applied=%v", res, err, r.applied)
	}
}

// D-18: the daily sweep re-redacts rows that slipped past the ingest check;
// a non-zero count is logged at warn.
func TestRedactionSweep(t *testing.T) {
	if _, err := RedactionSweep(context.Background(), &Context{}); err == nil {
		t.Error("missing RedactionTasks must fail")
	}
	boom := errors.New("db down")
	r := &fakeRedactions{sweepErr: boom}
	if _, err := RedactionSweep(context.Background(), retryCtx(r, nil, &levelLog{})); !errors.Is(err, boom) {
		t.Errorf("sweep error: %v", err)
	}

	r, log := &fakeRedactions{}, &levelLog{}
	c := retryCtx(r, nil, log)
	c.RedactionSweepWindow = 90 * 24 * time.Hour
	res, err := RedactionSweep(context.Background(), c)
	if err != nil || res != (Result{}) || log.infos != 1 || log.warns != 0 {
		t.Errorf("nothing to fix: res=%+v err=%v infos=%d warns=%d", res, err, log.infos, log.warns)
	}
	if r.window != 90*24*time.Hour {
		t.Errorf("window = %v", r.window)
	}

	r, log = &fakeRedactions{swept: 4}, &levelLog{}
	res, err = RedactionSweep(context.Background(), retryCtx(r, nil, log))
	if err != nil || res.Attempted != 4 || res.Succeeded != 4 || log.warns != 1 || log.infos != 0 {
		t.Errorf("fixed rows: res=%+v err=%v infos=%d warns=%d", res, err, log.infos, log.warns)
	}
}
