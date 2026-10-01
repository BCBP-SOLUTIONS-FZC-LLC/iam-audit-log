package metrics

import (
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
)

// Consumer adapts the Tier-1 dead-letter counter to the consumer fleet's
// metrics hook. platform-events' consumer records the rest of the
// consumption lifecycle itself (platform_messages_received/processed/
// failed_total, platform_retry_total, platform_event_propagation_seconds), so
// the service does not count those again. Register must have run.
type Consumer struct{}

// DeadLettered counts a message reaching the dead-letter threshold:
// platform_dlq_messages_total{operation="consume", event_type,
// reason="max_receive_count"}. eventType is already bounded
// (domain.EventTypeLabel).
func (Consumer) DeadLettered(eventType string) {
	DLQMessages.WithLabelValues(port.OperationConsume, eventType, port.ReasonMaxReceiveCount).Inc()
}
