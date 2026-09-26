# Monitoring — iam-audit-log

Prometheus alert rules for LLD §11 land in Phase 8 (`app-alerts.yml` +
`deploy/helm/templates/prometheusrule.yaml`). Non-negotiable thresholds already
fixed by the LLD:

| Signal | Severity |
|---|---|
| `iam_audit_log_dlq_messages_total{queue} > 0` (+ CloudWatch alarm, threshold 0) | Critical — compliance incident (RB-1) |
| `iam_audit_log_default_partition_rows_total > 0` | Critical (RB-3) |
| `iam_audit_log_archive_stalled` | Critical (RB-2) |
| `iam_audit_log_redaction_tasks_total{status="missed"}` | Critical (RB-7) |
| `iam_rls_violations_total{violation_type="cross_tenant_access"}` | Critical (RB-5) |
| `events_consumed_total{status="malformed"} > 0` | Critical — decision D-3 (platform-events deletes malformed envelopes) |
| `iam_audit_log_catalog_plans_stale_seconds` > 2× / 10× poll interval | Warning / Critical (RB-8) |
