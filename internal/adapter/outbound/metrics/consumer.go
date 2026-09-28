package metrics

import (
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
)

// Consumer adapts the Tier-1 message collectors to the consumer fleet's
// metrics hook. Labels follow the Platform Observability Registry: queue is
// the *-audit-q name, eventType is already bounded
// (domain.EventTypeLabel), and reason is one of the port.Reason* values.
// Register must have run.
type Consumer struct{}

// Received counts a dequeued message.
func (Consumer) Received(queue, eventType string) {
	MessagesReceived.WithLabelValues(queue, eventType).Inc()
}

// Processed counts a successfully handled message.
func (Consumer) Processed(eventType string) { MessagesProcessed.WithLabelValues(eventType).Inc() }

// Failed counts a handler failure. The message is left for SQS redelivery,
// so it is also a retry (platform_retry_total).
func (Consumer) Failed(eventType, reason string) {
	MessagesFailed.WithLabelValues(eventType, reason).Inc()
	RetryTotal.WithLabelValues(eventType, reason).Inc()
}

// DeadLettered counts a message reaching the dead-letter threshold.
func (Consumer) DeadLettered(queue string) {
	DLQMessages.WithLabelValues(queue, port.ReasonMaxReceiveExceeded).Inc()
}

// Propagated observes envelope time → persisted for a consumed event.
func (Consumer) Propagated(eventType string, took time.Duration) {
	EventPropagationSeconds.WithLabelValues(eventType).Observe(max(took, 0).Seconds())
}
