# Monitoring — iam-audit-log

The metrics follow the **Enterprise Platform Observability Standard** (LLD §11,
rev 0.26; BUILD_PLAN gap 46). Runbooks: [`docs/observability/runbooks.md`](../../docs/observability/runbooks.md) (per alert) and [`docs/runbook.md`](../../docs/runbook.md) (full procedures).

## Files

| File | What it is |
|---|---|
| `metric-registry.yaml` | **The inventory.** Every series: tier, type, labels, status (canonical / current / deprecated with replacement and sunset), plus the label vocabulary and forbidden label keys. |
| `metric-lint.yaml` | Which CI conformance checks run (flipping one to `false` disables it), gauge quantity tokens, forbidden keys. |
| `app-alerts.yml` | Alert rules (availability, messaging, ingest, storage, security, dependencies, reconciler). |
| `recording-rules.yml` | `service:<metric>:<agg>` recording rules that the alerts, SLOs and dashboard read. |
| `slo-rules.yml` | SLO error ratios (5m / 30m / 1h / 6h) and multi-window burn-rate alerts. |
| `dashboard-audit-log.json` | Grafana dashboard (uid `iam-audit-log`; variables environment, queue, event_type, dependency). |
| `prometheus-adapter-rule.yaml` | HPA metrics: `platform_http_requests_per_second` (pods) and `platform_queue_depth_audit_log` (external). |

`deploy/helm/templates/prometheusrule.yaml` (gated by `prometheusRule.enabled`) is
**generated** from the three rule files by `scripts/gen-prometheusrule.py`. The
HPA (`deploy/helm/templates/hpa.yaml`) can scale on queue backlog via
`autoscaling.targetQueueDepthPerReplica` (default off).

## Metric inventory

The generated inventory of every series, by tier, with each metric's registry status, labels and meaning, is [`docs/observability/metric-registry.md`](../../docs/observability/metric-registry.md) (`make metrics-inventory`; CI fails when it is stale). How the tiers are built and enforced is [`docs/observability/README.md`](../../docs/observability/README.md).

- **Labels.** Every series carries `{domain="iam", service="iam-audit-log", environment}`, injected centrally by platform-gincommon. `version` appears only on `platform_build_info`.
- **Tier 1 (`platform_*`).** The libraries record these. The service builds the five it records into from the Platform Observability Registry (`gincommon.NewShared*Vec`). Several are still `proposed`, and every rule on a proposed metric is marked `metric_status: proposed` (`metrics-registry-lint.sh`).
- **Tier 2 (`iam_*`).** `iam_rls_violations_total{violation_type}` is built from its registry entry (status `proposed`). It is fed from `rls_violation_log` through `audit_rls_violation_counts()` (migration 000010), exactly once fleet-wide.
- **Tier 3 (`iam_audit_log_*`).** 18 service-owned metrics, built with gincommon's validating constructors. The groups are:
  - ingest and direct-write;
  - query and export;
  - CAT-I2 catalog;
  - redaction;
  - archival and partitions.
- **Removed names.** No deprecated name is emitted. The gap-46 twins and the libraries' pre-standard names were removed outright, since nothing was deployed. See [`docs/observability/migration.md`](../../docs/observability/migration.md).

## Where the numbers come from

`cmd/server` is always scraped, so every Critical archival, redaction and queue
alert reads a gauge it publishes every `OPS_STATS_INTERVAL` (decision D-21): DB
state via `audit_ops_stats()` (aggregates only), SQS
`ApproximateNumberOfMessages` for each queue and its DLQ, and the RLS counts. The
reconciler CronJob's own counters are not scraped; a blocked or stalled run exits
non-zero, and the `kube_job_status_failed` alerts page on that (RB-9).

## Alerts

| Alert | Signal | Severity | Runbook |
|---|---|---|---|
| IAMAuditLogDown / ReplicasMissing | `up` | Critical / Warning | RB-9 |
| IAMAuditLogHighErrorRate | `service:platform_http_requests_error_ratio:5m` > 5% | Warning | — |
| IAMAuditLogDLQNotEmpty | `platform_dlq_depth{queue} > 0` (plus a CloudWatch alarm at 0) | Critical (compliance incident) | RB-1 |
| IAMAuditLogDeadLettered | `increase(platform_dlq_messages_total[15m]) > 0` by operation, event_type, reason | Critical | RB-1 |
| IAMAuditLogMessageFailures | `service:platform_messages_failure_ratio:5m` > 5% | Warning | RB-1 |
| IAMAuditLogRetrySpike | `service:platform_retry:rate5m` by operation > 0.5/s | Warning | RB-1 |
| IAMAuditLogQueueBacklog | `platform_queue_depth{queue}` > 1000 for 15 m | Warning | RB-1 |
| IAMAuditLogMalformedEnvelopes | `platform_messages_failed_total{reason="malformed"}` increase (forwarded to the DLQ, D-22) | Critical | RB-10 |
| IAMAuditLogUnknownEventTypes | `iam_audit_log_unknown_event_total` increase | Warning | — |
| IAMAuditLogIngestLagHigh | `service:iam_audit_log_ingest_lag:p99_15m` > 50 ms | Warning (SLO) | — |
| IAMAuditLogDirectWriteErrors | `directwrite_requests_total{result="error"}` > 5% | Warning | RB-6 |
| IAMAuditLogDefaultPartitionRows | `iam_audit_log_default_partition_rows > 0` | Critical | RB-3 |
| IAMAuditLogArchiveStalled | `iam_audit_log_archive_stalled_partitions > 0` | Critical | RB-2 |
| IAMAuditLogArchiveLag{Warning,Critical} | `iam_audit_log_archive_lag_seconds` > 1 d / > 3 d | Warning / Critical | RB-2 |
| IAMAuditLogRedactionPending | `iam_audit_log_redaction_pending_tasks > 0` for 15 m | Critical | RB-7 |
| IAMAuditLogCrossTenantAccess | `iam_rls_violations_total{violation_type="cross_tenant_access"}` | Critical | RB-5 |
| IAMAuditLogMissingGUC | `iam_rls_violations_total{violation_type="missing_or_invalid_guc"}` | Warning | RB-5 |
| IAMAuditLogDependencyErrors | `service:platform_dependency_request_error_ratio:10m` > 10% by dependency | Warning | RB-8 / RB-2 |
| IAMAuditLogCatalogPlansStale{Warning,Critical} | `iam_audit_log_catalog_plans_stale_seconds` > 1200 / > 6000 | Warning / Critical | RB-8 |
| IAMAuditLogReconcileJobFailed | `kube_job_status_failed` for `reconcile` | Critical | RB-9 |
| IAMAuditLogMaintenanceJobFailed | `kube_job_status_failed` for `redaction-retry`, `redaction-sweep`, `processed-events-prune` | Warning | RB-9 |

## SLOs (`slo-rules.yml`)

| SLO | Objective | Alerts |
|---|---|---|
| Ingest lag | 99% of rows persisted ≤ 50 ms after `occurred_at` (HLD §3.4) | IAMAuditLogIngestLagSLO{Fast,Slow}Burn |
| Event propagation | 99% of consumed events persisted ≤ 5 s after the envelope time | IAMAuditLogPropagationSLO{Fast,Slow}Burn |
| API availability | 99% of HTTP requests not 5xx | IAMAuditLogAvailabilitySLO{Fast,Slow}Burn |
| DLQ depth | `platform_dlq_depth == 0` always (no budget) | IAMAuditLogDLQNotEmpty; `slo:audit_log_dlq_depth:ok` records compliance |

Fast burn = 1 h and 5 m error ratios > 14.4× the budget (critical). Slow burn =
6 h and 30 m > 6× (warning).

`redaction_tasks_total{status="missed"}` is deliberately **not** an alert: under
AL-Q15 Option A (LLD rev 0.22) a subject with archived history is the expected
case. The CronJob alerts need kube-state-metrics. If `AUDIT_HOT_WINDOW_DAYS` or
`CATALOG_PLANS_POLL_INTERVAL` change, adjust the matching thresholds.

## CI checks

- `test/unit/metrics_registry_test.go`: every check in `metric-lint.yaml` (namespace classification, naming and suffixes, gauge quantity, required and centrally injected labels, shared-registry compliance, label-vocabulary compliance, forbidden keys, inventory completeness, deprecation metadata).
- `test/unit/observability_contract_test.go`: LLD §11 ↔ registered metrics; every expression in the alerts, recording rules, SLOs, dashboard and adapter rules reads a known, **non-deprecated** metric; the Helm PrometheusRule equals the three rule files.
- `.github/scripts/check-metric-naming.sh` (tier constructors, suffixes, prefix) and `.github/scripts/metrics-registry-lint.sh` (every live use of a proposed metric is marked).
- `.github/scripts/metricslint.sh` (`make metrics-lint`): platform-gincommon's `metricslint check` on a real scrape, `metricslint refs` on `deploy/` and the operator docs, and inventory drift for `docs/observability/metric-registry.md`.
