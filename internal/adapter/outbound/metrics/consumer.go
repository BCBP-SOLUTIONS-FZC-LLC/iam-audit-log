package metrics

// Consumer adapts the Tier-1 message collectors to the consumer fleet's
// metrics hook (queue label = the *-audit-q name). Register must have run.
type Consumer struct{}

// Received counts a dequeued message.
func (Consumer) Received(queue string) { MessagesReceived.WithLabelValues(queue).Inc() }

// Processed counts a successfully handled message.
func (Consumer) Processed(queue string) { MessagesProcessed.WithLabelValues(queue).Inc() }

// Failed counts a handler error (the message is redelivered).
func (Consumer) Failed(queue string) { MessagesFailed.WithLabelValues(queue).Inc() }

// DeadLettered counts a message reaching the dead-letter threshold.
func (Consumer) DeadLettered(queue string) { DLQMessages.WithLabelValues(queue).Inc() }
