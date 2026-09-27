package port

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
)

// IngestMetrics records the write path (LLD §11 Tier-3).
type IngestMetrics interface {
	// Ingested counts a newly persisted row (events_ingested_total) and
	// observes recorded_at − occurred_at (ingest_lag_seconds).
	Ingested(e domain.AuditEntry)
	// Duplicate counts a redelivery / replay (duplicate_events_total).
	Duplicate(consumer string)
	// Unknown counts a row persisted as <domain>.unknown (AL-EVT-4).
	Unknown(sourceService string)
	// DirectWrite counts one AL-5/AL-6 entry outcome:
	// created | duplicate | rejected | error.
	DirectWrite(sourceService, result string)
}

// QueryMetrics records the read path (LLD §11 Tier-3).
type QueryMetrics interface {
	// WindowClamped counts a query whose explicit range was cut to the plan
	// window (a product signal; a defaulted from is not counted).
	WindowClamped(planCode string)
	// ArchivedRead counts a read served (partly) from S3.
	ArchivedRead()
	// ExportJob counts an export lifecycle step: pending | ready | failed | expired.
	ExportJob(status string)
}

// OpsStatsReader reads the DB-derived operational state (audit_ops_stats(),
// migration 000009; gap 40): aggregate numbers only, no tenant data.
type OpsStatsReader interface {
	OpsStats(ctx context.Context, q domain.OpsStatsQuery) (domain.OpsStats, error)
}

// QueueDepths reads a queue's approximate visible depth (the DLQ gauges).
type QueueDepths interface {
	Depth(ctx context.Context, queueURL string) (int64, error)
}

// OpsMetrics publishes the DB- and SQS-derived gauges from cmd/server, which
// is always scraped (gap 40, decision D-21).
type OpsMetrics interface {
	SetOpsStats(s domain.OpsStats)
	SetDLQDepth(queue string, depth int64)
}
