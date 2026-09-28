package metrics

import (
	"testing"

	coredomain "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
)

// labeled returns a counter series matching every label pair (0 if none).
func labeled(t *testing.T, name string, want map[string]string) float64 {
	t.Helper()
	for _, m := range metricFamily(t, name).GetMetric() {
		match := 0
		for _, lp := range m.GetLabel() {
			if v, ok := want[lp.GetName()]; ok && v == lp.GetValue() {
				match++
			}
		}
		if match == len(want) {
			return m.GetCounter().GetValue()
		}
	}
	return 0
}

// LLD §11 archival collectors.
func TestArchiveAdapter(t *testing.T) {
	a := Archive{}
	a.PartitionArchived(coredomain.TierSecurity3y, "verified")
	a.Pruned(coredomain.TierAccess90d)
	a.RedactionBlocked()
	before := labeled(t, "iam_audit_log_archive_partitions_total", map[string]string{"tier": "security_3y", "result": "verified"})
	pruned := labeled(t, "iam_audit_log_retention_pruned_total", map[string]string{"tier": "access_90d"})
	blocked := metricFamily(t, "iam_audit_log_redaction_blocked_archive_total").GetMetric()[0].GetCounter().GetValue()

	a.PartitionArchived(coredomain.TierSecurity3y, "verified")
	a.PartitionArchived(coredomain.TierCompliance7y, "failed")
	a.Pruned(coredomain.TierAccess90d)
	a.RedactionBlocked()
	a.Stalled(3)

	if got := labeled(t, "iam_audit_log_archive_partitions_total", map[string]string{"tier": "security_3y", "result": "verified"}); got != before+1 {
		t.Errorf("archived = %v, want %v", got, before+1)
	}
	if labeled(t, "iam_audit_log_archive_partitions_total", map[string]string{"tier": "compliance_7y", "result": "failed"}) < 1 {
		t.Error("failed not counted")
	}
	if got := labeled(t, "iam_audit_log_retention_pruned_total", map[string]string{"tier": "access_90d"}); got != pruned+1 {
		t.Errorf("pruned = %v, want %v", got, pruned+1)
	}
	if got := metricFamily(t, "iam_audit_log_redaction_blocked_archive_total").GetMetric()[0].GetCounter().GetValue(); got != blocked+1 {
		t.Errorf("blocked = %v, want %v", got, blocked+1)
	}
	if got := gaugeValue(t, "iam_audit_log_archive_stalled_partitions"); got != 3 {
		t.Errorf("stalled = %v", got)
	}
	if got := gaugeValue(t, "iam_audit_log_archive_stalled"); got != 3 {
		t.Errorf("deprecated stalled twin = %v", got)
	}
	a.Stalled(0)
	if got := gaugeValue(t, "iam_audit_log_archive_stalled_partitions"); got != 0 {
		t.Errorf("stalled reset = %v", got)
	}
	if got := gaugeValue(t, "iam_audit_log_archive_stalled"); got != 0 {
		t.Errorf("deprecated stalled twin reset = %v", got)
	}
}
