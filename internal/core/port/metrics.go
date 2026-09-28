package port

import (
	"context"
	"time"

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
	// DuplicateMessage counts a redelivered bus message deduplicated by the
	// ledger (platform_duplicate_messages_total; eventType already bounded).
	DuplicateMessage(eventType string)
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
	// RLSViolations returns per-type counts of RLS violations logged since
	// the previous call, counted exactly once across all replicas.
	RLSViolations(ctx context.Context) (map[string]int64, error)
}

// QueueDepths reads a queue's approximate visible depth (the DLQ gauges).
type QueueDepths interface {
	Depth(ctx context.Context, queueURL string) (int64, error)
}

// OpsMetrics publishes the DB- and SQS-derived gauges from cmd/server, which
// is always scraped (gap 40, decision D-21).
type OpsMetrics interface {
	SetOpsStats(s domain.OpsStats)
	// SetQueueDepth / SetDLQDepth publish platform_queue_depth /
	// platform_dlq_depth for an inbound queue (label = the queue name).
	SetQueueDepth(queue string, depth int64)
	SetDLQDepth(queue string, depth int64)
	// AddRLSViolations feeds iam_rls_violations_total.
	AddRLSViolations(violationType string, n int64)
}

// Label vocabularies of the shared (platform_* / iam_*) metrics this service
// emits. They are part of the observability contract (Enterprise Platform
// Observability Standard, "Label governance") and mirror the allowed values
// in deploy/monitoring/metric-registry.yaml; TestMetricRegistry_* checks the
// two agree.
const (
	// ReasonInvalidEvent: the handler rejected the event (a domain error).
	ReasonInvalidEvent = "invalid_event"
	// ReasonDependencyUnavailable: a dependency (Postgres) was unreachable.
	ReasonDependencyUnavailable = "dependency_unavailable"
	// ReasonInternal: any other handler failure.
	ReasonInternal = "internal"
	// ReasonMaxReceiveExceeded: the message reached the dead-letter threshold.
	ReasonMaxReceiveExceeded = "max_receive_exceeded"

	// OutcomeSuccess / OutcomeError / OutcomeTimeout / OutcomeNotFound are the
	// platform_dependency_request_seconds outcomes.
	OutcomeSuccess  = "success"
	OutcomeError    = "error"
	OutcomeTimeout  = "timeout"
	OutcomeNotFound = "not_found"

	// Dependency and operation values for platform_dependency_request_seconds.
	DependencyCatalogAdmin = "catalog-admin"
	DependencyS3           = "s3"
	OperationListPlans     = "list_plans"
	OperationReadArchive   = "read_archive"
	OperationPutArchive    = "put_archive"
	OperationVerifyArchive = "verify_archive"
	OperationPutExport     = "put_export"
)

// DependencyObserver times one synchronous dependency call
// (platform_dependency_request_seconds{dependency,operation,outcome}).
type DependencyObserver interface {
	ObserveDependency(dependency, operation, outcome string, took time.Duration)
}
