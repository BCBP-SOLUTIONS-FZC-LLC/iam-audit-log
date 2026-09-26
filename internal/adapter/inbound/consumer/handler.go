package consumer

import (
	"context"
	"encoding/json"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
)

// BusIngester is the shared ingest path (implemented by
// *service.IngestService) — the same Append the direct-write endpoints use
// (AL-INV-2).
type BusIngester interface {
	IngestBus(ctx context.Context, ev domain.BusEvent, consumer string) (service.BusResult, error)
}

// Handler returns the platform-events handler for one queue. Returning an
// error leaves the message visible: SQS redelivers it (a transient DB
// failure heals; at-least-once, AL-INV-4) and after maxReceiveCount=5 it is
// redriven to <queue>-dlq for operator replay (RB-1) — never dropped.
func Handler(q Queue, ingest BusIngester) events.Handler {
	return func(ctx context.Context, env events.Envelope[json.RawMessage]) error {
		_, err := ingest.IngestBus(ctx, toBusEvent(q.Topic, env), q.Consumer)
		return err
	}
}

func toBusEvent(topic string, env events.Envelope[json.RawMessage]) domain.BusEvent {
	return domain.BusEvent{
		ID:        env.ID,
		Type:      env.Type,
		Source:    env.Source,
		Topic:     topic,
		TenantID:  env.TenantID,
		Subject:   env.Subject,
		Actor:     env.Actor,
		IPAddress: env.IPAddress,
		UserAgent: env.UserAgent,
		TraceID:   env.TraceID,
		Time:      env.Timestamp,
		Payload:   env.Payload,
	}
}
