package metrics

import "testing"

func redactionCount(t *testing.T, status string) float64 {
	t.Helper()
	for _, m := range metricFamily(t, "iam_audit_log_redaction_tasks_total").GetMetric() {
		for _, lp := range m.GetLabel() {
			if lp.GetName() == "status" && lp.GetValue() == status {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// LLD §11: iam_audit_log_redaction_tasks_total{status} (missed = Critical).
func TestRedactionAdapter_CountsByStatus(t *testing.T) {
	r := Redaction{}
	r.TaskOutcome("missed") // a CounterVec is gathered only once a series exists
	before := redactionCount(t, "missed")
	r.TaskOutcome("missed")
	r.TaskOutcome("applied")
	if got := redactionCount(t, "missed"); got != before+1 {
		t.Errorf("missed = %v, want %v", got, before+1)
	}
	if redactionCount(t, "applied") < 1 {
		t.Error("applied not counted")
	}
}
