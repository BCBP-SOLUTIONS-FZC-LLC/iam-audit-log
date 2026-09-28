package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
)

// fakeCatalog serves a scripted CAT-I2 response.
type fakeCatalog struct {
	mu          sync.Mutex
	plans       []port.CatalogPlan
	versions    map[string]int64
	err         error
	calls       int
	hadDeadline bool
	deadlineIn  time.Duration
}

func (c *fakeCatalog) Plans(ctx context.Context) ([]port.CatalogPlan, map[string]int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if dl, ok := ctx.Deadline(); ok {
		c.hadDeadline, c.deadlineIn = true, time.Until(dl)
	}
	return c.plans, c.versions, c.err
}

func (c *fakeCatalog) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// fakePollMetrics records poll outcomes.
type fakePollMetrics struct {
	results   []string
	succeeded []time.Time
}

func (m *fakePollMetrics) PollResult(result string, _ time.Duration) {
	m.results = append(m.results, result)
}
func (m *fakePollMetrics) PollSucceeded(at time.Time) { m.succeeded = append(m.succeeded, at) }

func catalogOf(days map[string]int, version int64) *fakeCatalog {
	c := &fakeCatalog{versions: map[string]int64{}}
	for code, d := range days {
		c.plans = append(c.plans, port.CatalogPlan{Code: code, AuditQueryWindowDays: d})
		c.versions[code] = version
	}
	return c
}

func newPoller(c *fakeCatalog, m *fakePollMetrics) *PlanPoller {
	return NewPlanPoller(c, m, PlanPollerConfig{Interval: time.Hour, Timeout: 2 * time.Second, Now: qClock}, &recLog{})
}

func assertWindow(t *testing.T, p *PlanPoller, code string, want int, wantOK bool) {
	t.Helper()
	if d, ok := p.WindowDays(code); d != want || ok != wantOK {
		t.Errorf("WindowDays(%q) = %d,%v; want %d,%v", code, d, ok, want, wantOK)
	}
}

func TestPlanPoller_ColdStartThenSuccess(t *testing.T) {
	c := catalogOf(map[string]int{"starter": 365, "enterprise": 2555}, 1)
	m := &fakePollMetrics{}
	p := newPoller(c, m)
	assertWindow(t, p, "starter", 0, false) // no map before the first poll (D-13 fallback applies)

	if err := p.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	assertWindow(t, p, "starter", 365, true)
	assertWindow(t, p, "enterprise", 2555, true)
	assertWindow(t, p, "unknown", 0, false)
	if fmt.Sprint(m.results) != "[success]" || len(m.succeeded) != 1 || !m.succeeded[0].Equal(qNow) {
		t.Errorf("metrics = %v / %v", m.results, m.succeeded)
	}
	if !c.hadDeadline || c.deadlineIn > 2*time.Second || c.deadlineIn <= 0 {
		t.Errorf("per-poll timeout not applied: deadline=%v in %v", c.hadDeadline, c.deadlineIn)
	}
}

// The map is swapped only when record_versions differ (AL-D15).
func TestPlanPoller_SwapsOnlyOnVersionChange(t *testing.T) {
	c := catalogOf(map[string]int{"pro": 1095}, 3)
	p := newPoller(c, &fakePollMetrics{})
	if err := p.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.plans = []port.CatalogPlan{{Code: "pro", AuditQueryWindowDays: 42}} // same versions → ignored
	if err := p.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertWindow(t, p, "pro", 1095, true)

	c.versions = map[string]int64{"pro": 4}
	if err := p.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertWindow(t, p, "pro", 42, true)
}

// Stale-if-error: failures and malformed responses keep the last good map.
func TestPlanPoller_StaleIfError(t *testing.T) {
	cases := []struct {
		name     string
		plans    []port.CatalogPlan
		versions map[string]int64
		err      error
		result   string
	}{
		{name: "transport error", err: errors.New("refused"), result: "error"},
		{name: "timeout sentinel", err: fmt.Errorf("wrap: %w", port.ErrCatalogTimeout), result: "timeout"},
		{name: "deadline exceeded", err: context.DeadlineExceeded, result: "timeout"},
		{name: "empty list", versions: map[string]int64{"pro": 9}, result: "error"},
		{name: "non-positive days", plans: []port.CatalogPlan{{Code: "pro", AuditQueryWindowDays: 0}}, versions: map[string]int64{"pro": 9}, result: "error"},
		{name: "empty code", plans: []port.CatalogPlan{{Code: "", AuditQueryWindowDays: 5}}, versions: map[string]int64{"": 9}, result: "error"},
		{name: "missing version", plans: []port.CatalogPlan{{Code: "pro", AuditQueryWindowDays: 5}, {Code: "ent", AuditQueryWindowDays: 9}}, versions: map[string]int64{"pro": 9}, result: "error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := catalogOf(map[string]int{"pro": 1095}, 1)
			m := &fakePollMetrics{}
			p := newPoller(c, m)
			if err := p.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			c.plans, c.versions, c.err = tc.plans, tc.versions, tc.err
			if err := p.Poll(context.Background()); err == nil {
				t.Fatal("expected an error")
			}
			assertWindow(t, p, "pro", 1095, true)
			if got := m.results[len(m.results)-1]; got != tc.result {
				t.Errorf("result = %s, want %s", got, tc.result)
			}
			if len(m.succeeded) != 1 {
				t.Errorf("a failed poll must not reset staleness: %v", m.succeeded)
			}
		})
	}
}

func TestPlanPoller_RunPollsImmediatelyThenOnInterval(t *testing.T) {
	c := catalogOf(map[string]int{"pro": 1095}, 1)
	c.err = errors.New("down") // failures are logged, never fatal
	p := NewPlanPoller(c, nil, PlanPollerConfig{Interval: 10 * time.Millisecond, Timeout: time.Millisecond}, &recLog{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); p.Run(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for c.callCount() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit on cancel")
	}
	if c.callCount() < 3 {
		t.Errorf("polls = %d, want ≥3 (immediate + interval)", c.callCount())
	}
}

func TestPlanPoller_RunLogsNothingAfterCancel(t *testing.T) {
	c := catalogOf(map[string]int{"pro": 1095}, 1)
	c.err = context.Canceled
	log := &recLog{}
	p := NewPlanPoller(c, nil, PlanPollerConfig{Interval: time.Hour}, log)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.Run(ctx) // polls once with a cancelled ctx, then returns
	if log.warns != 0 {
		t.Errorf("warns after cancel = %d", log.warns)
	}
}

func TestNewPlanPoller_Defaults(t *testing.T) {
	p := NewPlanPoller(&fakeCatalog{}, nil, PlanPollerConfig{}, nil)
	if p.cfg.Interval != 600*time.Second || p.cfg.Timeout != 3*time.Second || p.cfg.Now == nil {
		t.Errorf("defaults = %+v", p.cfg)
	}
	// nil metrics and logger are tolerated on every path.
	c := catalogOf(map[string]int{"pro": 1}, 1)
	p = NewPlanPoller(c, nil, PlanPollerConfig{}, nil)
	if err := p.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.err = errors.New("x")
	if err := p.Poll(context.Background()); err == nil {
		t.Fatal("want error")
	}
}

// D-13 precedence through the query path: live map → row value → default.
func TestPlanPoller_QueryWindowPrecedence_D13(t *testing.T) {
	c := catalogOf(map[string]int{"enterprise": 2555}, 1)
	poller := newPoller(c, &fakePollMetrics{})
	row := &domain.PlanWindowRow{PlanCode: "enterprise", QueryWindowDays: 30}
	r := &fakeReader{plan: row}
	svc := newQuery(r, nil, QueryConfig{DefaultWindowDays: 365, Windows: poller})

	from := func() time.Time {
		res, err := svc.Query(context.Background(), QueryRequest{TenantID: qTenant})
		if err != nil {
			t.Fatal(err)
		}
		return res.EffectiveFrom
	}
	if got, want := from(), qNow.AddDate(0, 0, -30); !got.Equal(want) {
		t.Errorf("cold start: effective_from = %v, want the row's 30 days %v", got, want)
	}
	if err := poller.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, want := from(), qNow.AddDate(0, 0, -2555); !got.Equal(want) {
		t.Errorf("live map: effective_from = %v, want %v", got, want)
	}
	row.PlanCode = "renamed"
	if got, want := from(), qNow.AddDate(0, 0, -30); !got.Equal(want) {
		t.Errorf("unrecognized plan: effective_from = %v, want the row's value %v", got, want)
	}
	r.plan = nil
	if got, want := from(), qNow.AddDate(0, 0, -365); !got.Equal(want) {
		t.Errorf("no row: effective_from = %v, want default %v", got, want)
	}
}
