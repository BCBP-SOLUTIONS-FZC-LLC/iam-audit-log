package metrics

import (
	coredomain "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
)

// Ingest implements port.IngestMetrics.
type Ingest struct{}

// Ingested counts a new row and observes its ingest lag.
func (Ingest) Ingested(e coredomain.AuditEntry) {
	EventsIngested.WithLabelValues(label(e.SourceService), string(e.IngestMode), e.EntryType).Inc()
	if !e.RecordedAt.IsZero() && !e.OccurredAt.IsZero() {
		IngestLag.Observe(max(e.RecordedAt.Sub(e.OccurredAt), 0).Seconds())
	}
}

// Duplicate counts a dedup hit.
func (Ingest) Duplicate(consumer string) { DuplicateEvents.WithLabelValues(consumer).Inc() }

// Unknown counts an unrecognized type persisted as <domain>.unknown.
func (Ingest) Unknown(sourceService string) {
	UnknownEvents.WithLabelValues(label(sourceService)).Inc()
}

// DirectWrite counts one AL-5/AL-6 entry outcome.
func (Ingest) DirectWrite(sourceService, result string) {
	DirectWriteCalls.WithLabelValues(label(sourceService), result).Inc()
}

// Query implements port.QueryMetrics.
type Query struct{}

// WindowClamped counts an explicit range cut to the plan window.
func (Query) WindowClamped(planCode string) {
	QueryWindowClamped.WithLabelValues(label(planCode)).Inc()
}

// ArchivedRead counts a read served from S3.
func (Query) ArchivedRead() { QueryArchivedReads.Inc() }

// ExportJob counts an export lifecycle step.
func (Query) ExportJob(status string) { ExportJobs.WithLabelValues(status).Inc() }

// Ops implements port.OpsMetrics (D-21).
type Ops struct{}

// SetOpsStats publishes the DB-derived gauges.
func (Ops) SetOpsStats(s coredomain.OpsStats) {
	DefaultPartitionRows.Set(float64(s.DefaultPartitionRows))
	ArchiveStalled.Set(float64(s.StalledPartitions))
	ArchiveLag.Set(s.ArchiveLagSeconds)
	PendingRedactions.Set(float64(s.PendingRedactions))
}

// SetDLQDepth publishes one DLQ's depth.
func (Ops) SetDLQDepth(queue string, depth int64) {
	DLQMessagesGauge.WithLabelValues(queue).Set(float64(depth))
}

// label bounds a caller-supplied label value (source_service, plan_code) so
// a bad producer cannot explode cardinality: lowercase slug, ≤ 64 chars,
// else "other"; empty becomes "none".
func label(v string) string {
	if v == "" {
		return "none"
	}
	if len(v) > 64 {
		return "other"
	}
	for _, r := range v {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '_' && r != '.' {
			return "other"
		}
	}
	return v
}
