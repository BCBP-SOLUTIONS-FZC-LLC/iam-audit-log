// Package consumer is the SQS consumer fleet (LLD §7.1): one platform-events
// consumer per inbound audit queue, each decoding the envelope (Glue codec
// keyed on `dataschema`, AL-D12), normalising it into the single AuditEntry
// shape and handing it to the shared ingest path (AL-INV-2). Consumption is
// at-least-once; dedup is the processed_events ledger inside that path
// (AL-INV-4).
package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
)

// dlqObserveReceiveCount is one delivery short of the queues' own
// maxReceiveCount=5 redrive policy (HLD §9.1), so the dead-letter handler
// observes each poison message before SQS itself moves it to <queue>-dlq
// (the sibling pattern). cmd/server overrides it from the platform-events
// loaded SQS_MAX_RECEIVE_COUNT via DeadLetterObserveAt.
const dlqObserveReceiveCount = 4

// DeadLetterObserveAt is the receive count at which the dead-letter handler
// fires for a queue whose SQS redrive policy is maxReceiveCount=redrive: one
// short, so SQS (not the library) performs the move (AL-EVT-4). An unset or
// too-small redrive falls back to the platform's 5 (LLD §12).
func DeadLetterObserveAt(redrive int) int {
	if redrive < 2 {
		redrive = dlqObserveReceiveCount + 1
	}
	return redrive - 1
}

// Builder constructs a platform-events consumer for a queue URL. cmd/server
// supplies it (the only place a raw *sqs.Client is built — arch-lint).
type Builder func(queueURL string, h events.Handler, opts ...events.ConsumerOption) (events.Consumer, error)

// Queue is one inbound queue the fleet consumes.
type Queue struct {
	Name        string // e.g. auth-audit-q
	URL         string
	Topic       string // logical topic, e.g. iam.auth.events
	Consumer    string // frozen processed_events.consumer discriminator
	Concurrency int
}

// Metrics is the Tier-1 instrumentation hook (nil-safe). platform-events'
// consumer records received / processed / failed / retry / propagation for
// every message itself, so the fleet only reports what the library cannot
// see: a message reaching the dead-letter threshold (eventType bounded by
// domain.EventTypeLabel).
type Metrics interface {
	DeadLettered(eventType string)
}

type running struct {
	name     string
	consumer events.Consumer
	exited   atomic.Bool
	err      atomic.Value // error
}

// Fleet runs every configured queue's consumer.
type Fleet struct {
	consumers []*running
	wg        sync.WaitGroup
}

// NewFleet builds one consumer per queue with a configured URL. handlerFor
// returns the queue's business handler; codec is the Glue decode codec.
// extra carries fleet-wide options (e.g. WithVisibilityTimeout from
// SQS_VISIBILITY_TIMEOUT).
func NewFleet(queues []Queue, build Builder, codec events.Codec, m Metrics, handlerFor func(Queue) events.Handler, extra ...events.ConsumerOption) (*Fleet, error) {
	f := &Fleet{}
	for _, q := range queues {
		if q.URL == "" {
			continue
		}
		conc := q.Concurrency
		if conc <= 0 {
			conc = 1
		}
		opts := append([]events.ConsumerOption{
			events.WithConsumerCodec(codec),
			events.WithConcurrency(conc),
			events.WithMaxReceiveCount(dlqObserveReceiveCount),
			events.WithDeadLetterHandler(deadLetter(q, m)),
		}, extra...)
		c, err := build(q.URL, handlerFor(q), opts...)
		if err != nil {
			return nil, fmt.Errorf("build %s consumer: %w", q.Name, err)
		}
		f.consumers = append(f.consumers, &running{name: q.Name, consumer: c})
	}
	return f, nil
}

// Len reports how many consumers the fleet runs.
func (f *Fleet) Len() int { return len(f.consumers) }

// Start launches every consumer on its own goroutine.
func (f *Fleet) Start(ctx context.Context, onExit func(queue string, err error)) {
	for _, r := range f.consumers {
		f.wg.Add(1)
		go func(r *running) {
			defer f.wg.Done()
			err := r.consumer.Start(ctx)
			if err != nil {
				r.err.Store(err)
			}
			r.exited.Store(true)
			if onExit != nil && err != nil && !errors.Is(err, context.Canceled) {
				onExit(r.name, err)
			}
		}(r)
	}
}

// Stop drains every consumer (in-flight handlers finish) and waits. The
// consumers stop concurrently: each Stop cancels its receive loop, then waits
// up to SQS_DRAIN_TIMEOUT (+5 s) for its handlers. Stopping them one after
// another would let the later queues keep receiving while the earlier ones
// drain, and would add up to 11 drain timeouts against
// terminationGracePeriodSeconds.
func (f *Fleet) Stop() error {
	errs := make([]error, len(f.consumers))
	var stopping sync.WaitGroup
	for i, r := range f.consumers {
		stopping.Go(func() {
			if err := r.consumer.Stop(); err != nil {
				errs[i] = fmt.Errorf("%s: %w", r.name, err)
			}
		})
	}
	stopping.Wait()
	f.wg.Wait()
	return errors.Join(errs...)
}

// Health is the /readyz consumer check (LLD §11): not ready if any
// consumer's receive loop has exited.
func (f *Fleet) Health(context.Context) error {
	for _, r := range f.consumers {
		if r.exited.Load() {
			if err, ok := r.err.Load().(error); ok && err != nil {
				return fmt.Errorf("%s consumer stopped: %w", r.name, err)
			}
			return fmt.Errorf("%s consumer stopped", r.name)
		}
	}
	return nil
}

// deadLetter is the dead-letter handler: it counts the message and returns
// an error so the SQS redrive (not the library) moves it to <queue>-dlq.
// platform-events therefore does not count it in platform_dlq_messages_total
// itself (it records platform_messages_failed_total{reason="dead_letter_error"}
// for the returned error); the service does, here.
func deadLetter(q Queue, m Metrics) events.Handler {
	return func(_ context.Context, env events.Envelope[json.RawMessage]) error {
		if m != nil {
			m.DeadLettered(domain.EventTypeLabel(q.Topic, env.Type))
		}
		return errors.New("dead-letter threshold reached — left for SQS redrive to " + q.Name + "-dlq")
	}
}
