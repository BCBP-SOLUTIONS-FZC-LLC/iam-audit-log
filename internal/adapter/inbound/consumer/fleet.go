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

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
)

// dlqObserveReceiveCount is one delivery short of the queues' own
// maxReceiveCount=5 redrive policy (HLD §9.1), so the dead-letter handler
// observes each poison message exactly once, on its last delivery, before
// SQS itself moves it to <queue>-dlq (the sibling pattern).
const dlqObserveReceiveCount = 4

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

// Metrics is the Tier-1 instrumentation hook (nil-safe).
type Metrics interface {
	Received(queue string)
	Processed(queue string)
	Failed(queue string)
	DeadLettered(queue string)
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
			events.WithDeadLetterHandler(deadLetter(q.Name, m)),
		}, extra...)
		c, err := build(q.URL, instrument(q.Name, m, handlerFor(q)), opts...)
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

// Stop drains every consumer (in-flight handlers finish) and waits.
func (f *Fleet) Stop() error {
	var errs []error
	for _, r := range f.consumers {
		if err := r.consumer.Stop(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", r.name, err))
		}
	}
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

func instrument(queue string, m Metrics, h events.Handler) events.Handler {
	return func(ctx context.Context, env events.Envelope[json.RawMessage]) error {
		if m != nil {
			m.Received(queue)
		}
		if err := h(ctx, env); err != nil {
			if m != nil {
				m.Failed(queue)
			}
			return err
		}
		if m != nil {
			m.Processed(queue)
		}
		return nil
	}
}

// deadLetter observes a poison message on its last delivery and returns an
// error so the message is NOT deleted: SQS redrives it to <queue>-dlq for
// operator replay (RB-1). A DLQ'd audit event is a compliance incident,
// never a degrade-to-stale (AL-EVT-4).
func deadLetter(queue string, m Metrics) events.Handler {
	return func(context.Context, events.Envelope[json.RawMessage]) error {
		if m != nil {
			m.DeadLettered(queue)
		}
		return errors.New("dead-letter threshold reached — left for SQS redrive to " + queue + "-dlq")
	}
}
