package metrics

// Redaction implements port.RedactionMetrics (LLD §11).
type Redaction struct{}

// TaskOutcome counts one redaction task outcome.
func (Redaction) TaskOutcome(status string) { RedactionTasks.WithLabelValues(status).Inc() }
