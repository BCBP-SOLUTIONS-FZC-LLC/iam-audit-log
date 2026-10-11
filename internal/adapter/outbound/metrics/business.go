// Package metrics registers this service's Prometheus collectors per the
// Enterprise Platform Observability Standard's three-tier taxonomy (LLD §11;
// deploy/monitoring/metric-registry.yaml is the inventory, checked by
// test/unit/metrics_registry_test.go):
//
//   - Tier 1 — platform_* : cross-domain concepts (message-consumption
//     lifecycle, DLQ, dependency calls). Built from their Platform
//     Observability Registry entry with gincommon.NewShared*Vec (name, help,
//     labels and buckets are the registry's), never with prometheus.New*.
//     The message-consumption lifecycle (received / processed / failed /
//     retry / propagation) is recorded by platform-events' consumer itself;
//     this package registers only the platform_* metrics the service records.
//   - Tier 2 — iam_* : concepts shared across IAM services (RLS violations),
//     built from the registry's IAM domain entry the same way.
//   - Tier 3 — iam_audit_log_* : behavior unique to this service (ingest,
//     archival, redaction, CAT-I2 poller — LLD §11 table), built with
//     gincommon.NewCounterVec / NewCounter / NewGauge / NewHistogramVec,
//     which validate the name, suffix and labels. The one exception is the
//     CatalogPlansStale GaugeFunc, which gincommon has no constructor for.
//
// Every collector carries gincommon's {domain, service, environment} const
// labels (gincommon.MetricsConstLabels), injected centrally, never by call
// sites; version is only on gincommon's platform_build_info.
//
// Every counter ends in _total, every histogram in _seconds, and every
// collector registers on gincommon.MetricsRegisterer() only — enforced by
// .github/scripts/check-metric-naming.sh and arch-lint.sh.
//
// Register must run after gincommon.ObservabilityMiddlewares and BEFORE
// events.InitMetrics / pgmetrics.InitWithIdentity: platform-events registers
// platform_dlq_messages_total, platform_duplicate_messages_total,
// platform_dependency_request_seconds, platform_queue_depth and
// platform_dlq_depth too, and reuses the collector already registered here
// (the registry desc is identical), so the library and the service write one
// series set.
package metrics

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

var (
	// ── Tier 1 — platform_* ─────────────────────────────────────────────

	// Names, help, label sets and buckets are the Platform Observability
	// Registry's (gincommon.NewShared*Vec). platform-events records the
	// consumption lifecycle (platform_messages_received/processed/failed_total,
	// platform_retry_total, platform_event_propagation_seconds) on the same
	// path, so the service does not record those itself.

	// DLQMessages — platform_dlq_messages_total{operation,event_type,reason}:
	// a message reaching the dead-letter threshold (operation="consume",
	// reason="max_receive_count"). The service's dead-letter handler leaves
	// the move to the SQS redrive, so platform-events does not count it. A
	// DLQ'd audit event is a compliance incident (AL-EVT-4, RB-1).
	DLQMessages *prometheus.CounterVec
	// DuplicateMessages — platform_duplicate_messages_total{queue,event_type}:
	// a redelivered bus message deduplicated by the service's processed_events
	// ledger (AL-INV-4; platform-events' inbox is not used).
	DuplicateMessages *prometheus.CounterVec
	// DependencyRequestSeconds —
	// platform_dependency_request_seconds{dependency,operation,outcome} for the
	// catalog-admin and S3 calls (platform-events adds sqs and codec).
	DependencyRequestSeconds *prometheus.HistogramVec
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
	IngestLag *prometheus.HistogramVec
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

var (
	registerOnce sync.Once
	registerErr  error
)

// buildError carries a constructor error from must to build.
type buildError struct{ err error }

// must returns c, or aborts the enclosing build with err.
func must[C any](c C, err error) C {
	mustOK(err)
	return c
}

// mustOK aborts the enclosing build with err when it is non-nil.
func mustOK(err error) {
	if err != nil {
		panic(buildError{err})
	}
}

// build runs f and returns the first error must / mustOK aborted it with.
// Any other panic propagates.
func build(f func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			be, ok := r.(buildError)
			if !ok {
				panic(r)
			}
			err = fmt.Errorf("metrics: register: %w", be.err)
		}
	}()
	f()
	return nil
}

// Register wires the collectors onto gincommon's registerer. Call once at
// startup AFTER gincommon.ObservabilityMiddlewares (it supplies the
// {domain, service, environment} const labels) and BEFORE events.InitMetrics,
// pgmetrics.InitWithIdentity and /metrics being served. Idempotent: later
// calls return the first call's result.
func Register() error {
	registerOnce.Do(func() { registerErr = registerMetrics() })
	return registerErr
}

func registerMetrics() error {
	// sLabels are the centrally injected {domain, service, environment}.
	sLabels := gincommon.MetricsConstLabels()
	if len(sLabels) == 0 {
		return fmt.Errorf("metrics: gincommon.ObservabilityMiddlewares must run before Register")
	}
	return build(func() { buildCollectors(sLabels) })
}

// buildCollectors constructs and registers every collector. A constructor
// error (a name, suffix or label the standard rejects, or a registration
// conflict) aborts it through must; build turns that into Register's error.
func buildCollectors(sLabels prometheus.Labels) {
	// ── Tier 1 — platform_* (registry-built) ─────────────────────────────
	DLQMessages = must(gincommon.NewSharedCounterVec(nil, "platform_dlq_messages_total"))
	DuplicateMessages = must(gincommon.NewSharedCounterVec(nil, "platform_duplicate_messages_total"))
	DependencyRequestSeconds = must(gincommon.NewSharedHistogramVec(nil, "platform_dependency_request_seconds"))
	QueueDepth = must(gincommon.NewSharedGaugeVec(nil, "platform_queue_depth"))
	DLQDepth = must(gincommon.NewSharedGaugeVec(nil, "platform_dlq_depth"))

	// ── Tier 2 — iam_* ───────────────────────────────────────────────────
	// Registry entry since platform-gincommon v1.5.0 (status proposed): name,
	// help and the violation_type vocabulary are the registry's, identical in
	// every IAM service that emits it.
	RLSViolations = must(gincommon.NewSharedCounterVec(nil, "iam_rls_violations_total"))

	// ── Tier 3 — iam_audit_log_* ─────────────────────────────────────────
	// gincommon's constructors validate the iam_audit_log_ prefix, the
	// _total/_seconds suffixes and prohibited labels, inject the identity
	// const labels and register on gincommon.MetricsRegisterer().
	CatalogPlansPolls = must(gincommon.NewCounterVec(nil,
		"iam_audit_log_catalog_plans_poll_total",
		"CAT-I2 plan-map poll outcomes, by result (success|error|timeout; AL-D15).",
		"result"))
	EventsIngested = must(gincommon.NewCounterVec(nil,
		"iam_audit_log_events_ingested_total",
		"Audit rows persisted, by source_service, ingest_mode and entry_type.",
		"source_service", "ingest_mode", "entry_type"))
	DuplicateEvents = must(gincommon.NewCounterVec(nil,
		"iam_audit_log_duplicate_events_total",
		"Dedup hits (redeliveries / idempotent replays), by consumer (AL-INV-4).",
		"consumer"))
	UnknownEvents = must(gincommon.NewCounterVec(nil,
		"iam_audit_log_unknown_event_total",
		"Unrecognized event types persisted as <domain>.unknown, by source_service (AL-EVT-4). Warning: extend the taxonomy.",
		"source_service"))
	IngestLag = must(gincommon.NewHistogramVec(nil,
		"iam_audit_log_ingest_lag_seconds",
		"recorded_at − occurred_at of newly persisted rows (HLD §3.4 SLO p99 < 50 ms async).",
		[]float64{0.01, 0.05, 0.1, 0.5, 1, 5, 30, 60, 300, 3600, 86400}))
	DirectWriteCalls = must(gincommon.NewCounterVec(nil,
		"iam_audit_log_directwrite_requests_total",
		"AL-5/AL-6 entry outcomes, by source_service and result (created|duplicate|rejected|error). RB-6.",
		"source_service", "result"))
	QueryWindowClamped = must(gincommon.NewCounterVec(nil,
		"iam_audit_log_query_window_clamped_total",
		"Queries whose explicit range was cut to the plan window, by plan_code (product signal, AL-INV-8).",
		"plan_code"))
	QueryArchivedReads = must(gincommon.NewCounter(nil,
		"iam_audit_log_query_archived_reads_total",
		"Reads served (partly) from the S3 archive."))
	ExportJobs = must(gincommon.NewCounterVec(nil,
		"iam_audit_log_export_jobs_total",
		"Export job lifecycle, by status (pending|ready|failed|expired).",
		"status"))
	DefaultPartitionRows = must(gincommon.NewGauge(nil,
		"iam_audit_log_default_partition_rows",
		"Rows in audit_events_default. Critical at > 0 (RB-3)."))
	ArchiveLag = must(gincommon.NewGauge(nil,
		"iam_audit_log_archive_lag_seconds",
		"Seconds past archival eligibility of the oldest still-attached partition (0 when none; RB-2)."))
	PendingRedactions = must(gincommon.NewGauge(nil,
		"iam_audit_log_redaction_pending_tasks",
		"Redaction tasks pending longer than OPS_REDACTION_PENDING_AGE. Critical at > 0 (RB-7)."))
	RedactionTasks = must(gincommon.NewCounterVec(nil,
		"iam_audit_log_redaction_tasks_total",
		"GDPR redaction task outcomes, by status (applied|not_applicable|missed|pending=apply failed). missed = archived rows retained (AL-Q15), routine; pending needs action (RB-7).",
		"status"))
	ArchivePartitions = must(gincommon.NewCounterVec(nil,
		"iam_audit_log_archive_partitions_total",
		"Per-tier archival outcomes, by tier and result (verified|failed).",
		"tier", "result"))
	RedactionBlockedArchive = must(gincommon.NewCounter(nil,
		"iam_audit_log_redaction_blocked_archive_total",
		"Archival refused because a pending redaction task touches the partition (AL-INV-12). Critical; near zero."))
	RetentionPruned = must(gincommon.NewCounterVec(nil,
		"iam_audit_log_retention_pruned_total",
		"Partition drops reconciled, by tier (§8.6).",
		"tier"))
	ArchiveStalled = must(gincommon.NewGauge(nil,
		"iam_audit_log_archive_stalled_partitions",
		"Eligible partitions still attached past OPS_ARCHIVE_STALL_GRACE (AL-INV-9 held). Critical when > 0."))

	catalogLastSuccess.Store(time.Now().UnixNano())
	CatalogPlansStale = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name:        "iam_audit_log_catalog_plans_stale_seconds",
		Help:        "Seconds since the last successful CAT-I2 poll (since start before the first). Stale-if-error masks outages (RB-8).",
		ConstLabels: sLabels,
	}, func() float64 { return time.Since(time.Unix(0, catalogLastSuccess.Load())).Seconds() })

	// gincommon has no GaugeFunc constructor: this one collector is built
	// with sLabels (the same identity const labels) and registered on
	// gincommon's registerer directly.
	mustOK(gincommon.MetricsRegisterer().Register(CatalogPlansStale))
}
