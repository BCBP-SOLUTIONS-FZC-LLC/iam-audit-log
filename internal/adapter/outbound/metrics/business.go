// Package metrics registers this service's Prometheus collectors per the
// Enterprise Platform Observability Standard's three-tier taxonomy (LLD §11;
// deploy/monitoring/metric-registry.yaml is the inventory, checked by
// test/unit/metrics_registry_test.go):
//
//   - Tier 1 — platform_* : cross-domain concepts (message-consumption
//     lifecycle, DLQ, dependency calls). "domain", "service", "environment"
//     are injected centrally as ConstLabels (platformLabels), never by call
//     sites.
//   - Tier 2 — iam_* : concepts shared across IAM services (RLS violations).
//     "service", "environment" injected centrally (serviceLabels).
//   - Tier 3 — iam_audit_log_* : behavior unique to this service (ingest,
//     archival, redaction, CAT-I2 poller — LLD §11 table). The CAT-I2 pair
//     landed in Phase 5; the rest land in Phase 8.
//
// Every counter ends in _total, every histogram in _seconds, and every
// collector registers on gincommon.MetricsRegisterer() only — enforced by
// .github/scripts/check-metric-naming.sh and arch-lint.sh.
package metrics

import (
	"maps"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

// domain is this service's fixed Observability Standard domain identity.
const domain = "iam"

// serviceName is the standard's `service` label value: the service's name
// within its domain (the domain is its own label on platform_* series and
// the iam_ prefix elsewhere). It overrides gincommon's app-name const
// label, as in iam-event-consumer (service="event-consumer"), so shared
// metrics aggregate by the same vocabulary across IAM services.
const serviceName = "audit-log"

var (
	// ── Tier 1 — platform_* ─────────────────────────────────────────────

	// Every name and label set below is Canonical in the Platform
	// Observability Registry (registry.go); labels are the registry's
	// approved vocabulary (queue, event_type, reason, dependency,
	// operation, outcome), each bounded.

	// MessagesReceived — platform_messages_received_total{queue,event_type}.
	MessagesReceived *prometheus.CounterVec
	// MessagesProcessed — platform_messages_processed_total{event_type}.
	MessagesProcessed *prometheus.CounterVec
	// MessagesFailed — platform_messages_failed_total{event_type,reason}.
	MessagesFailed *prometheus.CounterVec
	// RetryTotal — platform_retry_total{event_type,reason}: a failed message
	// left for SQS redelivery.
	RetryTotal *prometheus.CounterVec
	// DLQMessages — platform_dlq_messages_total{queue,reason}. A DLQ'd audit
	// event is a compliance incident (AL-EVT-4, RB-1).
	DLQMessages *prometheus.CounterVec
	// DuplicateMessages — platform_duplicate_messages_total{event_type}: a
	// redelivered bus message deduplicated by the ledger (AL-INV-4).
	DuplicateMessages *prometheus.CounterVec
	// DependencyRequestSeconds —
	// platform_dependency_request_seconds{dependency,operation,outcome}.
	DependencyRequestSeconds *prometheus.HistogramVec
	// EventPropagationSeconds — platform_event_propagation_seconds{event_type}:
	// envelope time → persisted.
	EventPropagationSeconds *prometheus.HistogramVec
	// QueueDepth — platform_queue_depth{queue}: visible depth of each inbound queue.
	QueueDepth *prometheus.GaugeVec
	// DLQDepth — platform_dlq_depth{queue}: visible depth of each inbound queue's DLQ.
	DLQDepth *prometheus.GaugeVec

	// ── Tier 2 — iam_* ──────────────────────────────────────────────────

	// RLSViolations is fed from rls_violation_log (LLD §4.3, §11):
	// cross_tenant_access = Critical, missing_or_invalid_guc = Warning.
	RLSViolations *prometheus.CounterVec

	// ── Tier 3 — iam_audit_log_* ────────────────────────────────────────

	// CatalogPlansPolls counts CAT-I2 plan-map polls by result
	// (success | error | timeout; AL-D15, LLD §11).
	CatalogPlansPolls *prometheus.CounterVec
	// CatalogPlansStale reports seconds since the last successful CAT-I2
	// poll (since process start before the first one) — Warning past 2×,
	// Critical past 10× CATALOG_PLANS_POLL_INTERVAL (RB-8).
	CatalogPlansStale prometheus.GaugeFunc

	// RedactionTasks counts GDPR redaction task outcomes by status
	// (applied | not_applicable | missed | pending = an apply that failed
	// and left the task for redaction-retry). missed is a routine outcome
	// (the subject also has archived rows, retained under AL-Q15 Option A),
	// not an alarm; pending is the one to alert on (RB-7).
	RedactionTasks *prometheus.CounterVec

	// ── Tier 3: write path ──

	// EventsIngested counts persisted rows {source_service,ingest_mode,entry_type}.
	EventsIngested *prometheus.CounterVec
	// DuplicateEvents counts dedup hits {consumer} (AL-INV-4).
	DuplicateEvents *prometheus.CounterVec
	// UnknownEvents counts rows persisted as <domain>.unknown {source_service}.
	UnknownEvents *prometheus.CounterVec
	// IngestLag observes recorded_at − occurred_at.
	IngestLag prometheus.Histogram
	// DirectWriteCalls counts AL-5/AL-6 entry outcomes {source_service,result}.
	DirectWriteCalls *prometheus.CounterVec

	// ── Tier 3: read path ──

	// QueryWindowClamped counts explicit ranges cut to the plan window {plan_code}.
	QueryWindowClamped *prometheus.CounterVec
	// QueryArchivedReads counts reads served from S3.
	QueryArchivedReads prometheus.Counter
	// ExportJobs counts the export lifecycle {status}.
	ExportJobs *prometheus.CounterVec

	// ── Tier 3: DB/SQS-derived gauges, set by cmd/server (D-21) ──

	// DefaultPartitionRows is the DEFAULT partition's row count — Critical at > 0 (RB-3).
	DefaultPartitionRows prometheus.Gauge

	// ── Deprecated (compatibility period; metric-registry.yaml records the
	// replacement and sunset). Emitted in parallel with their replacements
	// until dashboards, alerts, recording rules and SLOs have migrated.

	// LegacyDLQMessagesGauge — iam_audit_log_dlq_messages_total{queue="<q>-dlq"},
	// replaced by platform_dlq_depth{queue="<q>"} (a gauge must not end _total,
	// and DLQ depth is a Canonical platform concept).
	LegacyDLQMessagesGauge *prometheus.GaugeVec
	// LegacyDefaultPartitionRows — iam_audit_log_default_partition_rows_total,
	// replaced by iam_audit_log_default_partition_rows (a gauge must not end _total).
	LegacyDefaultPartitionRows prometheus.Gauge
	// LegacyArchiveStalled — iam_audit_log_archive_stalled, replaced by
	// iam_audit_log_archive_stalled_partitions (a gauge names its quantity).
	LegacyArchiveStalled prometheus.Gauge
	// ArchiveLag is the oldest attached eligible partition's age past eligibility (RB-2).
	ArchiveLag prometheus.Gauge
	// PendingRedactions counts stuck pending redaction tasks — Critical (RB-7).
	PendingRedactions prometheus.Gauge

	// ArchivePartitions counts per-tier archival outcomes (verified|failed).
	ArchivePartitions *prometheus.CounterVec
	// RedactionBlockedArchive counts archival refused for a pending
	// redaction task (AL-INV-12 backstop) — Critical, near zero.
	RedactionBlockedArchive prometheus.Counter
	// RetentionPruned counts partition drops per tier (§8.6).
	RetentionPruned *prometheus.CounterVec
	// ArchiveStalled is the number of eligible partitions past the stall
	// grace that are still attached (AL-INV-9) — Critical when > 0. cmd/server
	// sets it from audit_ops_stats() (D-21); the reconciler's own in-process
	// value is not scraped.
	ArchiveStalled prometheus.Gauge

	// catalogLastSuccess is the unix-nano time CatalogPlansStale measures from.
	catalogLastSuccess atomic.Int64
)

// platformLabels returns ConstLabels for a Tier-1 platform_* collector: the
// required {domain, service, environment} (injected here, never by call
// sites; standard naming rule 8) plus gincommon's version.
func platformLabels(environment string) prometheus.Labels {
	out := prometheus.Labels{}
	maps.Copy(out, gincommon.MetricsConstLabels()) // version
	out["domain"], out["service"], out["environment"] = domain, serviceName, environment
	return out
}

// serviceLabels returns ConstLabels for a Tier-2 (iam_*) or Tier-3
// (iam_audit_log_*) collector: the required {service, environment} plus
// gincommon's version — no domain label (the iam_ prefix encodes it).
func serviceLabels(environment string) prometheus.Labels {
	out := prometheus.Labels{}
	maps.Copy(out, gincommon.MetricsConstLabels()) // version
	out["service"], out["environment"] = serviceName, environment
	return out
}

var registerOnce sync.Once

// Register wires the collectors onto gincommon's registerer. Call once at
// startup AFTER gincommon.ObservabilityMiddlewares and BEFORE /metrics is
// served. Idempotent — only the first call's environment takes effect.
func Register(environment string) {
	registerOnce.Do(func() { registerMetrics(environment) })
}

func registerMetrics(environment string) {
	pLabels := platformLabels(environment)
	sLabels := serviceLabels(environment)

	// ── Tier 1 — platform_* ──────────────────────────────────────────────
	MessagesReceived = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "platform_messages_received_total",
		Help:        "Inbound queue messages dequeued, before processing, by queue and event_type.",
		ConstLabels: pLabels,
	}, []string{"queue", "event_type"})

	MessagesProcessed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "platform_messages_processed_total",
		Help:        "Inbound messages that completed processing successfully, by event_type.",
		ConstLabels: pLabels,
	}, []string{"event_type"})

	MessagesFailed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "platform_messages_failed_total",
		Help:        "Inbound messages whose processing failed, by event_type and reason.",
		ConstLabels: pLabels,
	}, []string{"event_type", "reason"})

	RetryTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "platform_retry_total",
		Help:        "Failed messages left for redelivery, by event_type and reason.",
		ConstLabels: pLabels,
	}, []string{"event_type", "reason"})

	DLQMessages = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "platform_dlq_messages_total",
		Help:        "Messages handed to the dead-letter path, by queue and reason. Any nonzero rate pages (AL-EVT-4).",
		ConstLabels: pLabels,
	}, []string{"queue", "reason"})

	DuplicateMessages = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "platform_duplicate_messages_total",
		Help:        "Redelivered messages deduplicated by the processed-message ledger, by event_type.",
		ConstLabels: pLabels,
	}, []string{"event_type"})

	DependencyRequestSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:        "platform_dependency_request_seconds",
		Help:        "Latency of synchronous dependency calls, by dependency, operation and outcome.",
		Buckets:     []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 3, 10},
		ConstLabels: pLabels,
	}, []string{"dependency", "operation", "outcome"})

	EventPropagationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:        "platform_event_propagation_seconds",
		Help:        "Envelope timestamp to persisted, for consumed events, by event_type.",
		Buckets:     []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 300},
		ConstLabels: pLabels,
	}, []string{"event_type"})

	QueueDepth = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name:        "platform_queue_depth",
		Help:        "Approximate visible depth of each inbound queue (HPA source).",
		ConstLabels: pLabels,
	}, []string{"queue"})

	DLQDepth = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name:        "platform_dlq_depth",
		Help:        "Approximate visible depth of each inbound queue's DLQ. Critical at > 0 (AL-EVT-4, RB-1).",
		ConstLabels: pLabels,
	}, []string{"queue"})

	// ── Tier 2 — iam_* ───────────────────────────────────────────────────
	RLSViolations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_rls_violations_total",
		Help:        "RLS policy violations recorded in rls_violation_log, by violation_type (cross_tenant_access|missing_or_invalid_guc).",
		ConstLabels: sLabels,
	}, []string{"violation_type"})

	// ── Tier 3 — iam_audit_log_* ─────────────────────────────────────────
	CatalogPlansPolls = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_audit_log_catalog_plans_poll_total",
		Help:        "CAT-I2 plan-map poll outcomes, by result (success|error|timeout; AL-D15).",
		ConstLabels: sLabels,
	}, []string{"result"})

	EventsIngested = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_audit_log_events_ingested_total",
		Help:        "Audit rows persisted, by source_service, ingest_mode and entry_type.",
		ConstLabels: sLabels,
	}, []string{"source_service", "ingest_mode", "entry_type"})

	DuplicateEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_audit_log_duplicate_events_total",
		Help:        "Dedup hits (redeliveries / idempotent replays), by consumer (AL-INV-4).",
		ConstLabels: sLabels,
	}, []string{"consumer"})

	UnknownEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_audit_log_unknown_event_total",
		Help:        "Unrecognized event types persisted as <domain>.unknown, by source_service (AL-EVT-4). Warning: extend the taxonomy.",
		ConstLabels: sLabels,
	}, []string{"source_service"})

	IngestLag = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:        "iam_audit_log_ingest_lag_seconds",
		Help:        "recorded_at − occurred_at of newly persisted rows (HLD §3.4 SLO p99 < 50 ms async).",
		Buckets:     []float64{0.01, 0.05, 0.1, 0.5, 1, 5, 30, 60, 300, 3600, 86400},
		ConstLabels: sLabels,
	})

	DirectWriteCalls = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_audit_log_directwrite_requests_total",
		Help:        "AL-5/AL-6 entry outcomes, by source_service and result (created|duplicate|rejected|error). RB-6.",
		ConstLabels: sLabels,
	}, []string{"source_service", "result"})

	QueryWindowClamped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_audit_log_query_window_clamped_total",
		Help:        "Queries whose explicit range was cut to the plan window, by plan_code (product signal, AL-INV-8).",
		ConstLabels: sLabels,
	}, []string{"plan_code"})

	QueryArchivedReads = prometheus.NewCounter(prometheus.CounterOpts{
		Name:        "iam_audit_log_query_archived_reads_total",
		Help:        "Reads served (partly) from the S3 archive.",
		ConstLabels: sLabels,
	})

	ExportJobs = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_audit_log_export_jobs_total",
		Help:        "Export job lifecycle, by status (pending|ready|failed|expired).",
		ConstLabels: sLabels,
	}, []string{"status"})

	DefaultPartitionRows = prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "iam_audit_log_default_partition_rows",
		Help:        "Rows in audit_events_default. Critical at > 0 (RB-3).",
		ConstLabels: sLabels,
	})

	LegacyDLQMessagesGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name:        "iam_audit_log_dlq_messages_total",
		Help:        "DEPRECATED — use platform_dlq_depth. Approximate visible depth of each *-audit-q-dlq.",
		ConstLabels: sLabels,
	}, []string{"queue"})

	LegacyDefaultPartitionRows = prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "iam_audit_log_default_partition_rows_total",
		Help:        "DEPRECATED — use iam_audit_log_default_partition_rows. Rows in audit_events_default.",
		ConstLabels: sLabels,
	})

	LegacyArchiveStalled = prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "iam_audit_log_archive_stalled",
		Help:        "DEPRECATED — use iam_audit_log_archive_stalled_partitions.",
		ConstLabels: sLabels,
	})

	ArchiveLag = prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "iam_audit_log_archive_lag_seconds",
		Help:        "Seconds past archival eligibility of the oldest still-attached partition (0 when none; RB-2).",
		ConstLabels: sLabels,
	})

	PendingRedactions = prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "iam_audit_log_redaction_pending_tasks",
		Help:        "Redaction tasks pending longer than OPS_REDACTION_PENDING_AGE. Critical at > 0 (RB-7).",
		ConstLabels: sLabels,
	})

	RedactionTasks = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_audit_log_redaction_tasks_total",
		Help:        "GDPR redaction task outcomes, by status (applied|not_applicable|missed|pending=apply failed). missed = archived rows retained (AL-Q15), routine; pending needs action (RB-7).",
		ConstLabels: sLabels,
	}, []string{"status"})

	ArchivePartitions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_audit_log_archive_partitions_total",
		Help:        "Per-tier archival outcomes, by tier and result (verified|failed).",
		ConstLabels: sLabels,
	}, []string{"tier", "result"})

	RedactionBlockedArchive = prometheus.NewCounter(prometheus.CounterOpts{
		Name:        "iam_audit_log_redaction_blocked_archive_total",
		Help:        "Archival refused because a pending redaction task touches the partition (AL-INV-12). Critical; near zero.",
		ConstLabels: sLabels,
	})

	RetentionPruned = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_audit_log_retention_pruned_total",
		Help:        "Partition drops reconciled, by tier (§8.6).",
		ConstLabels: sLabels,
	}, []string{"tier"})

	ArchiveStalled = prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "iam_audit_log_archive_stalled_partitions",
		Help:        "Eligible partitions still attached past OPS_ARCHIVE_STALL_GRACE (AL-INV-9 held). Critical when > 0.",
		ConstLabels: sLabels,
	})

	catalogLastSuccess.Store(time.Now().UnixNano())
	CatalogPlansStale = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name:        "iam_audit_log_catalog_plans_stale_seconds",
		Help:        "Seconds since the last successful CAT-I2 poll (since start before the first). Stale-if-error masks outages (RB-8).",
		ConstLabels: sLabels,
	}, func() float64 { return time.Since(time.Unix(0, catalogLastSuccess.Load())).Seconds() })

	gincommon.MetricsRegisterer().MustRegister(
		MessagesReceived, MessagesProcessed, MessagesFailed, RetryTotal, DLQMessages, DuplicateMessages,
		DependencyRequestSeconds, EventPropagationSeconds, QueueDepth, DLQDepth,
		RLSViolations,
		CatalogPlansPolls, CatalogPlansStale, RedactionTasks,
		ArchivePartitions, RedactionBlockedArchive, RetentionPruned, ArchiveStalled,
		EventsIngested, DuplicateEvents, UnknownEvents, IngestLag, DirectWriteCalls,
		QueryWindowClamped, QueryArchivedReads, ExportJobs,
		DefaultPartitionRows, ArchiveLag, PendingRedactions,
		LegacyDLQMessagesGauge, LegacyDefaultPartitionRows, LegacyArchiveStalled,
	)
}
