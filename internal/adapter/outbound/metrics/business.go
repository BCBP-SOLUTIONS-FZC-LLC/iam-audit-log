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
//     archival, redaction, CAT-I2 poller — LLD §11 table). Landed in Phase 8.
//
// Every counter ends in _total, every histogram in _seconds, and every
// collector registers on gincommon.MetricsRegisterer() only — enforced by
// .github/scripts/check-metric-naming.sh and arch-lint.sh.
package metrics

import (
	"maps"
	"sync"

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

	gincommon.MetricsRegisterer().MustRegister(
		MessagesReceived, MessagesProcessed, MessagesFailed, DLQMessages,
		DependencyRequestSeconds, RLSViolations,
	)
}
