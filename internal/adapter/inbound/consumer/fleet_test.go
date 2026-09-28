package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
)

type fakeConsumer struct {
	startErr error
	block    bool
	stopped  chan struct{}
	stopErr  error
}

func (f *fakeConsumer) Start(ctx context.Context) error {
	if f.block {
		select {
		case <-ctx.Done():
		case <-f.stopped:
		}
		return ctx.Err()
	}
	return f.startErr
}
func (f *fakeConsumer) Stop() error {
	select {
	case <-f.stopped:
	default:
		close(f.stopped)
	}
	return f.stopErr
}

type recMetrics struct {
	mu                              sync.Mutex
	received, processed, failed, dl int
	propagated                      int
	lastQueue, lastType, lastReason string
}

func (m *recMetrics) Received(q, et string) {
	m.mu.Lock()
	m.received++
	m.lastQueue, m.lastType = q, et
	m.mu.Unlock()
}
func (m *recMetrics) Processed(et string) { m.mu.Lock(); m.processed++; m.lastType = et; m.mu.Unlock() }
func (m *recMetrics) Failed(et, reason string) {
	m.mu.Lock()
	m.failed++
	m.lastType, m.lastReason = et, reason
	m.mu.Unlock()
}
func (m *recMetrics) DeadLettered(string) { m.mu.Lock(); m.dl++; m.mu.Unlock() }
func (m *recMetrics) Propagated(string, time.Duration) {
	m.mu.Lock()
	m.propagated++
	m.mu.Unlock()
}

type captured struct {
	url     string
	handler events.Handler
	opts    int
}

func TestNewFleet_OneConsumerPerConfiguredQueue(t *testing.T) {
	var got []captured
	build := func(url string, h events.Handler, opts ...events.ConsumerOption) (events.Consumer, error) {
		got = append(got, captured{url: url, handler: h, opts: len(opts)})
		return &fakeConsumer{block: true, stopped: make(chan struct{})}, nil
	}
	queues := []Queue{
		{Name: "auth-audit-q", URL: "http://q/auth", Topic: domain.TopicAuth, Consumer: "auth"},
		{Name: "user-audit-q", Topic: domain.TopicUser, Consumer: "user"}, // no URL → skipped
	}
	f, err := NewFleet(queues, build, nil, nil, func(Queue) events.Handler { return nil }, events.WithVisibilityTimeout(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if f.Len() != 1 || got[0].url != "http://q/auth" || got[0].opts != 5 {
		t.Fatalf("fleet = %d, captured = %+v", f.Len(), got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	f.Start(ctx, nil)
	if err := f.Health(ctx); err != nil {
		t.Fatalf("running fleet must be healthy: %v", err)
	}
	cancel()
	if err := f.Stop(); err != nil {
		t.Fatal(err)
	}

	if _, err := NewFleet(queues, func(string, events.Handler, ...events.ConsumerOption) (events.Consumer, error) {
		return nil, errors.New("bad queue")
	}, nil, nil, func(Queue) events.Handler { return nil }); err == nil {
		t.Error("builder failure must surface")
	}
}

// §11 readiness: a consumer whose receive loop exits makes /readyz fail.
func TestFleet_HealthReportsExitedConsumer(t *testing.T) {
	boom := errors.New("receive loop died")
	build := func(string, events.Handler, ...events.ConsumerOption) (events.Consumer, error) {
		return &fakeConsumer{startErr: boom, stopped: make(chan struct{}), stopErr: errors.New("stop failed")}, nil
	}
	f, _ := NewFleet([]Queue{{Name: "tender-audit-q", URL: "u"}}, build, nil, nil, func(Queue) events.Handler { return nil })
	exited := make(chan string, 1)
	f.Start(context.Background(), func(q string, err error) { exited <- q })
	if q := <-exited; q != "tender-audit-q" {
		t.Fatalf("onExit queue = %s", q)
	}
	if err := f.Health(context.Background()); err == nil || !errors.Is(err, boom) {
		t.Fatalf("health = %v", err)
	}
	if err := f.Stop(); err == nil {
		t.Error("stop errors must be joined and returned")
	}

	clean, _ := NewFleet([]Queue{{Name: "q", URL: "u"}}, func(string, events.Handler, ...events.ConsumerOption) (events.Consumer, error) {
		return &fakeConsumer{stopped: make(chan struct{})}, nil
	}, nil, nil, func(Queue) events.Handler { return nil })
	clean.Start(context.Background(), nil)
	time.Sleep(20 * time.Millisecond)
	if err := clean.Health(context.Background()); err == nil {
		t.Error("an exited consumer (even without error) is not ready")
	}
}

// Instrumentation + the dead-letter observer (never deletes: returns error
// so SQS redrives to <queue>-dlq — AL-EVT-4).
func TestInstrumentAndDeadLetter_ALEVT4(t *testing.T) {
	m := &recMetrics{}
	q := Queue{Name: "q", Topic: domain.TopicUser}
	ok := instrument(q, m, func(context.Context, events.Envelope[json.RawMessage]) error { return nil })
	bad := instrument(q, m, func(context.Context, events.Envelope[json.RawMessage]) error { return errors.New("x") })
	_ = ok(context.Background(), events.Envelope[json.RawMessage]{})
	_ = bad(context.Background(), events.Envelope[json.RawMessage]{})
	if m.received != 2 || m.processed != 1 || m.failed != 1 {
		t.Errorf("metrics = %+v", m)
	}
	if err := deadLetter("q", m)(context.Background(), events.Envelope[json.RawMessage]{}); err == nil || m.dl != 1 {
		t.Error("dead-letter handler must count and return an error")
	}
	_ = instrument(q, nil, func(context.Context, events.Envelope[json.RawMessage]) error { return errors.New("x") })(context.Background(), events.Envelope[json.RawMessage]{})
	_ = instrument(q, nil, func(context.Context, events.Envelope[json.RawMessage]) error { return nil })(context.Background(), events.Envelope[json.RawMessage]{})
	_ = deadLetter("q", nil)(context.Background(), events.Envelope[json.RawMessage]{})
}

type fakeIngester struct {
	got      domain.BusEvent
	consumer string
	err      error
}

func (f *fakeIngester) IngestBus(_ context.Context, ev domain.BusEvent, consumer string) (service.BusResult, error) {
	f.got, f.consumer = ev, consumer
	return service.BusResult{}, f.err
}

// The handler maps every envelope field and the queue's topic/consumer.
func TestHandler_MapsEnvelope(t *testing.T) {
	ing := &fakeIngester{}
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	h := Handler(Queue{Topic: domain.TopicAuth, Consumer: "auth"}, ing)
	env := events.Envelope[json.RawMessage]{ID: "i", Type: "LoginSuccess", Source: "iam-event-consumer", TenantID: "t",
		Subject: "s", Actor: "a", IPAddress: "1.2.3.4", UserAgent: "ua", TraceID: "tr", Timestamp: at, Payload: json.RawMessage(`{}`)}
	if err := h(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	want := domain.BusEvent{ID: "i", Type: "LoginSuccess", Source: "iam-event-consumer", Topic: domain.TopicAuth, TenantID: "t",
		Subject: "s", Actor: "a", IPAddress: "1.2.3.4", UserAgent: "ua", TraceID: "tr", Time: at, Payload: json.RawMessage(`{}`)}
	if string(ing.got.Payload) != "{}" || ing.consumer != "auth" {
		t.Fatalf("got %+v", ing.got)
	}
	ing.got.Payload, want.Payload = nil, nil
	if !reflect.DeepEqual(ing.got, want) {
		t.Errorf("got %+v\nwant %+v", ing.got, want)
	}
	ing.err = errors.New("db down")
	if err := h(context.Background(), env); err == nil {
		t.Error("errors must propagate so SQS redelivers")
	}
}

// The dead-letter handler fires one receive short of the queues' SQS redrive
// count (loaded by platform-events), so SQS performs the move (AL-EVT-4).
func TestDeadLetterObserveAt(t *testing.T) {
	for redrive, want := range map[int]int{5: 4, 3: 2, 2: 1, 1: 4, 0: 4, -1: 4} {
		if got := DeadLetterObserveAt(redrive); got != want {
			t.Errorf("DeadLetterObserveAt(%d) = %d, want %d", redrive, got, want)
		}
	}
}

// Tier-1 labels are bounded: a type known to the topic's taxonomy is kept,
// anything else becomes "unknown"; the queue label is the queue name.
func TestInstrument_EventTypeBounding(t *testing.T) {
	q := Queue{Name: "user-audit-q", Topic: domain.TopicUser}
	ok := func(context.Context, events.Envelope[json.RawMessage]) error { return nil }
	for typ, want := range map[string]string{
		"UserDeleted":      "UserDeleted",
		"TotallyMadeUp":    "unknown",
		"TenantOffboarded": "unknown", // a real type, but of another topic
		"":                 "unknown",
	} {
		m := &recMetrics{}
		_ = instrument(q, m, ok)(context.Background(), events.Envelope[json.RawMessage]{Type: typ})
		if m.lastType != want || m.lastQueue != "user-audit-q" {
			t.Errorf("%q: event_type=%q queue=%q, want %q", typ, m.lastType, m.lastQueue, want)
		}
	}
}

// Propagation is observed only for a processed message with an envelope time.
func TestInstrument_PropagatedOnSuccessWithTimestamp(t *testing.T) {
	q := Queue{Name: "q", Topic: domain.TopicUser}
	m := &recMetrics{}
	ok := instrument(q, m, func(context.Context, events.Envelope[json.RawMessage]) error { return nil })
	bad := instrument(q, m, func(context.Context, events.Envelope[json.RawMessage]) error { return errors.New("x") })
	_ = ok(context.Background(), events.Envelope[json.RawMessage]{})                                         // no timestamp
	_ = bad(context.Background(), events.Envelope[json.RawMessage]{Timestamp: time.Now().Add(-time.Second)}) // failed
	if m.propagated != 0 {
		t.Fatalf("propagated = %d, want 0", m.propagated)
	}
	_ = ok(context.Background(), events.Envelope[json.RawMessage]{Timestamp: time.Now().Add(-time.Second)})
	if m.propagated != 1 {
		t.Fatalf("propagated = %d, want 1", m.propagated)
	}
}

// failureReason maps handler errors onto the registry's reason vocabulary.
func TestFailureReason(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"dependency": {domain.NewError(domain.ErrDependencyUnavailable, "db down"), "dependency_unavailable"},
		"wrapped":    {fmt.Errorf("ingest: %w", domain.NewError(domain.ErrDependencyUnavailable, "db")), "dependency_unavailable"},
		"domain":     {domain.NewError(domain.ErrInvalidRequest, "bad"), "invalid_event"},
		"plain":      {errors.New("boom"), "internal"},
	} {
		if got := failureReason(tc.err); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
		m := &recMetrics{}
		_ = instrument(Queue{Name: "q", Topic: domain.TopicUser}, m,
			func(context.Context, events.Envelope[json.RawMessage]) error { return tc.err })(context.Background(), events.Envelope[json.RawMessage]{})
		if m.lastReason != tc.want {
			t.Errorf("%s: instrument reason %q, want %q", name, m.lastReason, tc.want)
		}
	}
}
