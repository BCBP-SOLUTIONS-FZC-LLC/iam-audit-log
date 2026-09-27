package metrics

import (
	"strings"
	"testing"
	"time"

	coredomain "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
)

// labeledGauge returns a gauge series matching every label pair (-1 if none).
func labeledGauge(t *testing.T, name string, want map[string]string) float64 {
	t.Helper()
	for _, m := range metricFamily(t, name).GetMetric() {
		match := 0
		for _, lp := range m.GetLabel() {
			if v, ok := want[lp.GetName()]; ok && v == lp.GetValue() {
				match++
			}
		}
		if match == len(want) {
			return m.GetGauge().GetValue()
		}
	}
	return -1
}

func histogram(t *testing.T, name string) (count uint64, sum float64) {
	t.Helper()
	h := metricFamily(t, name).GetMetric()[0].GetHistogram()
	return h.GetSampleCount(), h.GetSampleSum()
}

func counter(t *testing.T, name string) float64 {
	t.Helper()
	return metricFamily(t, name).GetMetric()[0].GetCounter().GetValue()
}

// label() bounds caller-supplied label values so a bad producer cannot
// explode cardinality.
func TestLabel_Bounds(t *testing.T) {
	for in, want := range map[string]string{
		"":                       "none",
		"iam-catalog-admin":      "iam-catalog-admin",
		"plan_2.pro":             "plan_2.pro",
		"Iam-Catalog":            "other", // upper case
		"iam catalog":            "other", // space
		"x@y":                    "other",
		"ünïcode":                "other",
		strings.Repeat("a", 64):  strings.Repeat("a", 64),
		strings.Repeat("a", 65):  "other",
		"a/b":                    "other",
		"0123456789-abcdefghijk": "0123456789-abcdefghijk",
	} {
		if got := label(in); got != want {
			t.Errorf("label(%q) = %q, want %q", in, got, want)
		}
	}
}

// Write-path adapter (LLD §11 Tier-3).
func TestIngestAdapter(t *testing.T) {
	a := Ingest{}
	now := time.Now().UTC()
	e := coredomain.AuditEntry{
		SourceService: "iam-user-profile", IngestMode: coredomain.IngestBus, EntryType: "user.updated",
		OccurredAt: now.Add(-2 * time.Second), RecordedAt: now,
	}
	lbl := map[string]string{"source_service": "iam-user-profile", "ingest_mode": "bus", "entry_type": "user.updated"}
	// Prime every vector series: a Vec has no family until first use.
	a.Ingested(e)
	a.Duplicate("user")
	a.Unknown("")
	a.DirectWrite("iam-tender-acl", "rejected")
	before := labeled(t, "iam_audit_log_events_ingested_total", lbl)
	c0, s0 := histogram(t, "iam_audit_log_ingest_lag_seconds")
	a.Ingested(e)
	if got := labeled(t, "iam_audit_log_events_ingested_total", lbl); got != before+1 {
		t.Errorf("ingested = %v, want %v", got, before+1)
	}
	c1, s1 := histogram(t, "iam_audit_log_ingest_lag_seconds")
	if c1 != c0+1 || s1-s0 < 1.9 || s1-s0 > 2.5 {
		t.Errorf("lag: count %d→%d sum %v→%v (want +1, +~2s)", c0, c1, s0, s1)
	}

	// A producer clock ahead of ours (recorded before occurred) is observed
	// as 0, never negative.
	ahead := e
	ahead.OccurredAt = now.Add(time.Hour)
	a.Ingested(ahead)
	c2, s2 := histogram(t, "iam_audit_log_ingest_lag_seconds")
	if c2 != c1+1 || s2 != s1 {
		t.Errorf("negative lag must observe 0: count %d→%d sum %v→%v", c1, c2, s1, s2)
	}
	// Missing times are counted but not observed.
	noTime := e
	noTime.RecordedAt = time.Time{}
	a.Ingested(noTime)
	if c3, _ := histogram(t, "iam_audit_log_ingest_lag_seconds"); c3 != c2 {
		t.Errorf("zero recorded_at must not observe lag: %d→%d", c2, c3)
	}

	// Caller label values are bounded.
	bad := e
	bad.SourceService = "Bad Service!"
	otherBefore := labeled(t, "iam_audit_log_events_ingested_total", map[string]string{"source_service": "other", "ingest_mode": "bus", "entry_type": "user.updated"})
	a.Ingested(bad)
	if got := labeled(t, "iam_audit_log_events_ingested_total", map[string]string{"source_service": "other", "ingest_mode": "bus", "entry_type": "user.updated"}); got != otherBefore+1 {
		t.Errorf("bounded source_service: %v", got)
	}

	dupBefore := labeled(t, "iam_audit_log_duplicate_events_total", map[string]string{"consumer": "user"})
	a.Duplicate("user")
	if got := labeled(t, "iam_audit_log_duplicate_events_total", map[string]string{"consumer": "user"}); got != dupBefore+1 {
		t.Errorf("duplicate = %v", got)
	}
	unkBefore := labeled(t, "iam_audit_log_unknown_event_total", map[string]string{"source_service": "none"})
	a.Unknown("")
	if got := labeled(t, "iam_audit_log_unknown_event_total", map[string]string{"source_service": "none"}); got != unkBefore+1 {
		t.Errorf("unknown(empty→none) = %v", got)
	}
	dwBefore := labeled(t, "iam_audit_log_directwrite_requests_total", map[string]string{"source_service": "iam-tender-acl", "result": "rejected"})
	a.DirectWrite("iam-tender-acl", "rejected")
	if got := labeled(t, "iam_audit_log_directwrite_requests_total", map[string]string{"source_service": "iam-tender-acl", "result": "rejected"}); got != dwBefore+1 {
		t.Errorf("directwrite = %v", got)
	}
}

// Read-path adapter.
func TestQueryAdapter(t *testing.T) {
	q := Query{}
	q.WindowClamped("starter")
	q.ArchivedRead()
	q.ExportJob("ready")
	clamped := labeled(t, "iam_audit_log_query_window_clamped_total", map[string]string{"plan_code": "starter"})
	archived := counter(t, "iam_audit_log_query_archived_reads_total")
	ready := labeled(t, "iam_audit_log_export_jobs_total", map[string]string{"status": "ready"})

	q.WindowClamped("starter")
	q.WindowClamped("") // no tenant_plan_window row
	q.ArchivedRead()
	q.ExportJob("ready")
	if got := labeled(t, "iam_audit_log_query_window_clamped_total", map[string]string{"plan_code": "starter"}); got != clamped+1 {
		t.Errorf("clamped = %v", got)
	}
	if labeled(t, "iam_audit_log_query_window_clamped_total", map[string]string{"plan_code": "none"}) < 1 {
		t.Error(`empty plan code must be labeled "none"`)
	}
	if got := counter(t, "iam_audit_log_query_archived_reads_total"); got != archived+1 {
		t.Errorf("archived = %v", got)
	}
	if got := labeled(t, "iam_audit_log_export_jobs_total", map[string]string{"status": "ready"}); got != ready+1 {
		t.Errorf("export ready = %v", got)
	}
}

// Ops gauges (D-21): each value set, and set again (a gauge, not a counter).
func TestOpsAdapter(t *testing.T) {
	o := Ops{}
	o.SetOpsStats(coredomain.OpsStats{DefaultPartitionRows: 3, StalledPartitions: 2, ArchiveLagSeconds: 7200.5, PendingRedactions: 4})
	for name, want := range map[string]float64{
		"iam_audit_log_default_partition_rows_total": 3,
		"iam_audit_log_archive_stalled":              2,
		"iam_audit_log_archive_lag_seconds":          7200.5,
		"iam_audit_log_redaction_pending_tasks":      4,
	} {
		if got := gaugeValue(t, name); got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	o.SetOpsStats(coredomain.OpsStats{})
	for _, name := range []string{"iam_audit_log_default_partition_rows_total", "iam_audit_log_archive_stalled",
		"iam_audit_log_archive_lag_seconds", "iam_audit_log_redaction_pending_tasks"} {
		if got := gaugeValue(t, name); got != 0 {
			t.Errorf("%s reset = %v", name, got)
		}
	}

	o.SetDLQDepth("user-audit-q-dlq", 5)
	o.SetDLQDepth("auth-audit-q-dlq", 0)
	if got := labeledGauge(t, "iam_audit_log_dlq_messages_total", map[string]string{"queue": "user-audit-q-dlq"}); got != 5 {
		t.Errorf("dlq user = %v", got)
	}
	o.SetDLQDepth("user-audit-q-dlq", 1)
	if got := labeledGauge(t, "iam_audit_log_dlq_messages_total", map[string]string{"queue": "user-audit-q-dlq"}); got != 1 {
		t.Errorf("dlq user after drain = %v", got)
	}
	if got := labeledGauge(t, "iam_audit_log_dlq_messages_total", map[string]string{"queue": "auth-audit-q-dlq"}); got != 0 {
		t.Errorf("dlq auth = %v", got)
	}
}
