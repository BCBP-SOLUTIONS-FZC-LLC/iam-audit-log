// Package metrics registers this service's Prometheus collectors per the IAM
// Platform Observability Standard's three-tier hierarchy (LLD §11), mirroring
// iam-org-membership's business.go:
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

var (
	// ── Tier 1 — platform_* ─────────────────────────────────────────────

	// MessagesReceived counts inbound SQS messages dequeued, by queue (the
	// 11 *-audit-q queues, LLD §7.1).
	MessagesReceived *prometheus.CounterVec
	// MessagesProcessed counts inbound messages processed successfully, by queue.
	MessagesProcessed *prometheus.CounterVec
	// MessagesFailed counts inbound messages whose processing errored, by queue.
	MessagesFailed *prometheus.CounterVec

	// DLQMessages counts messages handed to the dead-letter path, by queue.
	// A DLQ'd audit event is a compliance incident (AL-EVT-4, RB-1).
	DLQMessages *prometheus.CounterVec

	// DependencyRequestSeconds times synchronous dependency calls (today
	// only the CAT-I2 plans poller, AL-D15), by target_service/endpoint.
	DependencyRequestSeconds *prometheus.HistogramVec

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

	// DLQMessagesGauge is each DLQ's depth {queue} — Critical at > 0 (AL-EVT-4).
	DLQMessagesGauge *prometheus.GaugeVec
	// DefaultPartitionRows is the DEFAULT partition's row count — Critical at > 0 (RB-3).
	DefaultPartitionRows prometheus.Gauge
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

// platformLabels returns ConstLabels for a Tier-1 platform_* collector:
// domain, environment, and gincommon's {service, version}.
func platformLabels(environment string) prometheus.Labels {
	out := prometheus.Labels{"domain": domain, "environment": environment}
	maps.Copy(out, gincommon.MetricsConstLabels())
	return out
}

// serviceLabels returns ConstLabels for a Tier-2 (iam_*) or Tier-3
// (iam_audit_log_*) collector: environment plus gincommon's
// {service, version} — no domain label (the iam_ prefix encodes it).
func serviceLabels(environment string) prometheus.Labels {
	out := prometheus.Labels{"environment": environment}
	maps.Copy(out, gincommon.MetricsConstLabels())
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
		Help:        "Inbound queue messages dequeued, by queue, before processing.",
		ConstLabels: pLabels,
	}, []string{"queue"})

	MessagesProcessed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "platform_messages_processed_total",
		Help:        "Inbound queue messages that completed processing successfully, by queue.",
		ConstLabels: pLabels,
	}, []string{"queue"})

	MessagesFailed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "platform_messages_failed_total",
		Help:        "Inbound queue messages whose processing returned an error, by queue.",
		ConstLabels: pLabels,
	}, []string{"queue"})

	DLQMessages = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "platform_dlq_messages_total",
		Help:        "Messages handed to the dead-letter path, by queue. Any nonzero rate pages (AL-EVT-4).",
		ConstLabels: pLabels,
	}, []string{"queue"})

	DependencyRequestSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:        "platform_dependency_request_seconds",
		Help:        "Latency of synchronous dependency calls, by target_service and endpoint.",
		Buckets:     []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 3},
		ConstLabels: pLabels,
	}, []string{"target_service", "endpoint"})

	// ── Tier 2 — iam_* ───────────────────────────────────────────────────
	RLSViolations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_rls_violations_total",
		Help:        "Sampled RLS policy violations from rls_violation_log, by violation_type.",
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

	DLQMessagesGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name:        "iam_audit_log_dlq_messages_total",
		Help:        "Approximate visible depth of each *-audit-q-dlq. Critical at > 0 (a potentially lost audit record, AL-EVT-4, RB-1).",
		ConstLabels: sLabels,
	}, []string{"queue"})

	DefaultPartitionRows = prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "iam_audit_log_default_partition_rows_total",
		Help:        "Rows in audit_events_default. Critical at > 0 (RB-3).",
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
		Name:        "iam_audit_log_archive_stalled",
		Help:        "Eligible partitions the last reconcile run could not drop (AL-INV-9 held). Critical when > 0.",
		ConstLabels: sLabels,
	})

	catalogLastSuccess.Store(time.Now().UnixNano())
	CatalogPlansStale = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name:        "iam_audit_log_catalog_plans_stale_seconds",
		Help:        "Seconds since the last successful CAT-I2 poll (since start before the first). Stale-if-error masks outages (RB-8).",
		ConstLabels: sLabels,
	}, func() float64 { return time.Since(time.Unix(0, catalogLastSuccess.Load())).Seconds() })

	gincommon.MetricsRegisterer().MustRegister(
		MessagesReceived, MessagesProcessed, MessagesFailed, DLQMessages,
		DependencyRequestSeconds, RLSViolations,
		CatalogPlansPolls, CatalogPlansStale, RedactionTasks,
		ArchivePartitions, RedactionBlockedArchive, RetentionPruned, ArchiveStalled,
		EventsIngested, DuplicateEvents, UnknownEvents, IngestLag, DirectWriteCalls,
		QueryWindowClamped, QueryArchivedReads, ExportJobs,
		DLQMessagesGauge, DefaultPartitionRows, ArchiveLag, PendingRedactions,
	)
}
