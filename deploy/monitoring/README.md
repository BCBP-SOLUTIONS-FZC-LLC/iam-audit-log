# Monitoring — iam-audit-log

Alert rules for LLD §11 live in `deploy/helm/templates/prometheusrule.yaml`
(gated by `prometheusRule.enabled`, default on). `app-alerts.yml` is its static
mirror for a Prometheus that is not using the operator CRDs, rendered with the
release name `iam-audit-log`. Runbooks: [`docs/runbook.md`](../../docs/runbook.md).

Two unit tests keep this directory honest: `TestAlerts_StaticMirrorMatchesHelm`
checks the mirror equals the template, and `TestAlerts_ReferenceKnownMetrics`
checks every alert reads a metric this service or a shared library actually
registers.

## Where the numbers come from

`cmd/server` is always scraped, so every Critical archival, redaction and DLQ
alert reads a gauge it publishes every `OPS_STATS_INTERVAL` (decision D-21):

- DB state via `audit_ops_stats()` (migration 000009; aggregate numbers only, no
  tenant data);
- SQS `ApproximateNumberOfMessages` on each `<queue>-dlq`.

The reconciler CronJob's own counters (`archive_partitions_total`,
`retention_pruned_total`, `redaction_blocked_archive_total`) are not scraped. A
blocked or stalled run exits non-zero instead, and the `kube_job_status_failed`
alerts page on that (RB-9).

| Alert | Signal | Severity | Runbook |
|---|---|---|---|
| IAMAuditLogDown / ReplicasMissing | `up` | Critical / Warning | RB-9 |
| IAMAuditLogHighErrorRate | `http_requests_total{status_class="5xx"}` > 5% | Warning | — |
| IAMAuditLogDLQNotEmpty | `iam_audit_log_dlq_messages_total{queue} > 0` (plus a CloudWatch alarm at 0) | Critical (compliance incident) | RB-1 |
| IAMAuditLogMalformedEnvelopes | `events_consumed_total{status="malformed"}` increase | Critical (D-3) | RB-10 |
| IAMAuditLogUnknownEventTypes | `iam_audit_log_unknown_event_total` increase | Warning | — |
| IAMAuditLogIngestLagHigh | p99 `iam_audit_log_ingest_lag_seconds` > 50 ms for 15 m | Warning (SLO) | — |
| IAMAuditLogDirectWriteErrors | `directwrite_requests_total{result="error"}` > 5% | Warning | RB-6 |
| IAMAuditLogDefaultPartitionRows | `iam_audit_log_default_partition_rows_total > 0` | Critical | RB-3 |
| IAMAuditLogArchiveStalled | `iam_audit_log_archive_stalled > 0` | Critical | RB-2 |
| IAMAuditLogArchiveLag{Warning,Critical} | `iam_audit_log_archive_lag_seconds` > 1 d / > 3 d | Warning / Critical | RB-2 |
| IAMAuditLogRedactionPending | `iam_audit_log_redaction_pending_tasks > 0` for 15 m | Critical | RB-7 |
| IAMAuditLogCrossTenantAccess | `iam_rls_violations_total{violation_type="cross_tenant_access"}` | Critical | RB-5 |
| IAMAuditLogMissingGUC | `iam_rls_violations_total{violation_type="missing_or_invalid_guc"}` | Warning | RB-5 |
| IAMAuditLogCatalogPlansStale{Warning,Critical} | `iam_audit_log_catalog_plans_stale_seconds` > 1200 / > 6000 (2× / 10× the 600 s interval) | Warning / Critical | RB-8 |
| IAMAuditLogReconcileJobFailed | `kube_job_status_failed` for `reconcile` | Critical | RB-9 |
| IAMAuditLogMaintenanceJobFailed | `kube_job_status_failed` for `redaction-retry`, `redaction-sweep`, `processed-events-prune` | Warning | RB-9 |

`redaction_tasks_total{status="missed"}` is deliberately **not** an alert: under
AL-Q15 Option A (LLD rev 0.22) a subject with archived history is the expected
case, not an incident.

The CronJob alerts need kube-state-metrics. If `AUDIT_HOT_WINDOW_DAYS` or
`CATALOG_PLANS_POLL_INTERVAL` change, adjust the matching thresholds here.
