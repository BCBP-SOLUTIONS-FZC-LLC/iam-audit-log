# Runbooks: iam-audit-log alerts

This page has one entry for every alert in [`deploy/monitoring/app-alerts.yml`](../../deploy/monitoring/app-alerts.yml) and [`deploy/monitoring/slo-rules.yml`](../../deploy/monitoring/slo-rules.yml), which [`make prometheusrule`](../../scripts/gen-prometheusrule.py) mirrors into the Helm [`PrometheusRule`](../../deploy/helm/templates/prometheusrule.yaml).

- **Headings.** Each heading is the alert name. Each entry gives the meaning, the triage queries and the mitigation, then points to the full operator procedure in [`../runbook.md`](../runbook.md) (RB-1 … RB-10). The alert's `runbook_url` annotation is an absolute link to that RB section, or to this page for the four alerts with no RB procedure; `TestAlerts_RunbookURLsResolve` checks every anchor.
- **Labels.** Every alert on a service metric selects `domain="iam"`, `service="iam-audit-log"` and keeps `environment`. The alerts with `metric_status: proposed` read a registry metric that is not ratified yet (see [README.md](README.md#proposed-metrics-in-live-rules)).
- **Library alerts.** Platform-level alerts on the libraries' own metrics are documented in platform-gincommon's `docs/observability/runbooks.md`.

Two standing rules apply to every alert: **never purge a DLQ**, and **never delete or rewrite an audit row by hand**.

Common starting points:

- Rollouts: `count by (version) (platform_build_info{service="iam-audit-log", environment="$environment"})`
- Logs (Loki): `{service="iam-audit-log"} | json`, then follow `trace_id` into Tempo. Log lines carry IDs only, never payload or metadata.
- Readiness: `GET /readyz` checks the database (`audit_app` pool health) and the consumer fleet.
- The archival, redaction, DEFAULT-partition, queue and RLS gauges are published by `cmd/server` every `OPS_STATS_INTERVAL` (60 s) from database and SQS state (D-21). They keep updating whether or not a reconciler job ran.

## Availability

### IAMAuditLogDown

**Meaning.** No pod has been scraped as healthy for 2 minutes. Audit events wait in SQS (not lost; 14 days including the DLQ), and direct-write producers retry with the same `Idempotency-Key`.

**Triage.**
1. Run `kubectl -n iam get pods -l app.kubernetes.io/name=iam-audit-log`, then `kubectl describe pod` to find crash loops and probe failures.
2. Startup exits are deliberate for bad configuration. The `invalid configuration` log line names the variable, for example a non-platform `APP_ENV` or a missing queue URL outside dev.
3. Outside dev, the app pool must authenticate as `audit_app` without `BYPASSRLS`, or startup refuses (AL-INV-3).
4. Migrations run at startup as `audit_migrator`: check for an advisory-lock wait or a dirty migration.

**Mitigate.** Roll back the last deploy, or fix the named configuration. Full procedure: [RB-9](../runbook.md#rb-9--reconciler-cronjob-failed--service-down).

### IAMAuditLogReplicasMissing

**Meaning.** Fewer than 2 replicas are healthy (baseline 3). One more disruption takes ingestion and the query API to zero.

**Triage.** As for `IAMAuditLogDown`, per pod. Check node pressure and pending pods.

### IAMAuditLogHighErrorRate

`metric_status: proposed`

**Meaning.** More than 5% of HTTP requests returned 5xx over 5 minutes (`service:platform_http_requests_error_ratio:5m`).

**Triage.**
1. Find the routes: `sum by (route, error_class) (rate(platform_http_requests_total{service="iam-audit-log", status_class="5xx"}[5m]))`.
2. The error body's `code` gives the cause:
   - `dependency_unavailable`: Postgres or PgBouncer (a closed pool, connection or resource SQLSTATE), or S3 for archived reads and exports. Check `platform_db_pool_connections`, `platform_db_pool_empty_acquires_total` and `service:platform_dependency_request_error_ratio:10m{dependency="s3"}`.
   - `internal_error`: an unhandled error, logged at error level with the request's `trace_id`.
3. Correlate with the availability SLO (`IAMAuditLogAvailabilitySLO*`).

**Mitigate.** Roll back a bad version, or restore the failing dependency.

## Messaging

### IAMAuditLogDLQNotEmpty

`metric_status: proposed`

**Meaning.** `platform_dlq_depth{queue} > 0`: one or more messages sit in `<queue>-dlq`. Each is a potentially lost audit record (AL-EVT-4), so this is a **compliance incident**.

**Triage.**
1. Find the cause class:
   - failures by reason: `sum by (reason) (service:platform_messages_failed:rate5m{service="iam-audit-log"})`
   - backlog per queue: `max by (queue) (platform_queue_depth{service="iam-audit-log"})`
2. Read the DLQ **without deleting** (`--visibility-timeout 0`). Classify each message as one of:
   - a schema or decode failure;
   - a poison payload;
   - an outage that exhausted `SQS_MAX_RECEIVE_COUNT`.
3. Check whether the event is already stored (`audit_events.source_event_id = <envelope id>`). A replay is always safe (AL-INV-4).

**Mitigate.** Fix the dependency or the producer, then redrive with `aws sqs start-message-move-task`. Never edit the message body. Full procedure: [RB-1](../runbook.md#rb-1--dlq-has-messages).

### IAMAuditLogDeadLettered

**Meaning.** `platform_dlq_messages_total` increased in the last 15 minutes. A message reached the dead-letter threshold, and this service's dead-letter handler counted it once (`operation="consume"`, `reason="max_receive_count"`). SQS then redrives it, and `IAMAuditLogDLQNotEmpty` follows for its queue.

**Triage.** The counter has no `queue` label (registry label set). The `dead-letter handler invoked` log line names the queue and message id. Continue as for `IAMAuditLogDLQNotEmpty`.

**Mitigate.** [RB-1](../runbook.md#rb-1--dlq-has-messages).

### IAMAuditLogMessageFailures

**Meaning.** More than 5% of received messages failed over 5 minutes, for 10 minutes (`service:platform_messages_failure_ratio:5m`). Failed messages are redelivered and, if they keep failing, dead-lettered.

**Triage.** Break down by reason: `sum by (reason, event_type) (rate(platform_messages_failed_total{service="iam-audit-log"}[5m]))`.
- `handler_error`: the ingest path failed. It is usually Postgres (`dependency_unavailable` in the logs), or an event the taxonomy cannot store.
- `decode_error`: Glue schema validation. The producer shipped a payload its registered schema rejects, or the schema version cannot be resolved (`glue codec: …`). Errors carry instance paths and keywords only.
- `malformed`: not an envelope. See `IAMAuditLogMalformedEnvelopes`.

**Mitigate.** Restore Postgres, or fix the producer. [RB-1](../runbook.md#rb-1--dlq-has-messages).

### IAMAuditLogRetrySpike

**Meaning.** More than 0.5 redeliveries per second for 10 minutes (`service:platform_retry:rate5m`). Sustained retries end in the DLQ after `SQS_MAX_RECEIVE_COUNT` deliveries.

**Triage.** `sum by (operation, event_type) (rate(platform_retry_total{service="iam-audit-log"}[5m]))`, then continue as for `IAMAuditLogMessageFailures`.

### IAMAuditLogQueueBacklog

`metric_status: proposed`

**Meaning.** More than 1000 messages have waited on one inbound queue for 15 minutes (`platform_queue_depth`). Consumers are slow or down.

**Triage.**
1. Replicas and readiness (`/readyz` `consumers`).
2. Postgres latency: `histogram_quantile(0.99, sum by (le, operation) (rate(platform_db_query_duration_seconds_bucket{service="iam-audit-log"}[5m])))` and pool waits.
3. The failure ratio.
4. Producer bursts: `sum by (source_service) (rate(iam_audit_log_events_ingested_total{service="iam-audit-log"}[5m]))`.

**Mitigate.** Scale out (the HPA can scale on `platform_queue_depth_audit_log` via `autoscaling.targetQueueDepthPerReplica`), or fix the slow dependency.

### IAMAuditLogMalformedEnvelopes

**Meaning.** `platform_messages_failed_total{reason="malformed"}` increased: a message body was not a valid envelope. platform-events forwards it to its queue's DLQ (`DLQReason` starts with `malformed message body`, decision D-22), so `IAMAuditLogDLQNotEmpty` follows. Nothing is lost, but a producer is emitting a broken envelope.

**Triage.** Read the DLQ copy without deleting it. `OriginalQueue` names the queue, and the queue names the producer. The library logs only the body's size and SHA-256, not the body.

**Mitigate.** The producer fixes its envelope serialisation and re-emits the affected events from its outbox (a re-emit with the same envelope id is deduplicated). Do not redrive a malformed body; it can never be ingested. Full procedure: [RB-10](../runbook.md#rb-10--malformed-envelopes).

## Ingest

### IAMAuditLogUnknownEventTypes

**Meaning.** Events from `source_service` were persisted as `<domain>.unknown` in the last hour (`iam_audit_log_unknown_event_total`). They are captured, not lost (AL-EVT-4), at the fail-safe `security_3y` tier, but without a real entry type.

**Triage.** `sum by (source_service) (increase(iam_audit_log_unknown_event_total{service="iam-audit-log"}[1h]))`, then find the new type in the stored rows' `source_event_type` (the producer's verbatim type), where `entry_type` is `<domain>.unknown`.

**Mitigate.** Add the type to `internal/core/domain/taxonomy.go` (with its tier), to `api/asyncapi.yaml` and as an LLD §7.1 row (see `.claude/CLAUDE.md`, "How to add an entry_type").

### IAMAuditLogIngestLagHigh

**Meaning.** p99 `recorded_at − occurred_at` has been above the 50 ms target for 15 minutes (`service:iam_audit_log_ingest_lag:p99_15m`, HLD §3.4).

**Triage.**
1. The SQS backlog (`platform_queue_depth`).
2. Consumer health.
3. Postgres write latency (`platform_db_query_duration_seconds{operation="INSERT"}`).
4. Propagation (`service:platform_event_propagation:p99_5m`).

Producer clock skew inflates the lag as well, and so does a burst replay of old events, since their `occurred_at` is far in the past.

### IAMAuditLogDirectWriteErrors

**Meaning.** More than 5% of AL-5/AL-6 direct-write entries failed server-side over 10 minutes.

**Triage.** `sum by (source_service, result) (rate(iam_audit_log_directwrite_requests_total{service="iam-audit-log"}[10m]))`.
- `error` (5xx) usually means Postgres is unavailable.
- `rejected` (4xx) is the producer's fault: `unknown_entry_type`, `invalid_actor`, `metadata_too_large` or `forbidden_peer`.

**Mitigate.** Producers keep their own business writes and retry with the same `Idempotency-Key`. Full procedure: [RB-6](../runbook.md#rb-6--direct-write-producer-failing).

## Storage

### IAMAuditLogDefaultPartitionRows

**Meaning.** `iam_audit_log_default_partition_rows > 0`: rows fell outside every explicit monthly partition into `audit_events_default`. The cause is a bad producer clock or a very late replay.

**Triage.** Group the DEFAULT rows by month and `source_service`, and check whether the month was dropped (`audit_event_archive_state`).

**Mitigate.**
- **A dropped month** is re-opened automatically by the next reconcile run (D-19).
- **A never-partitioned month** needs the producer's clock fixed, and a DB-owner re-home. Never `DELETE`.

Full procedure: [RB-3](../runbook.md#rb-3--rows-in-audit_events_default).

### IAMAuditLogArchiveStalled

**Meaning.** `iam_audit_log_archive_stalled_partitions > 0` for 30 minutes. A partition is past archival eligibility plus `OPS_ARCHIVE_STALL_GRACE` and still attached. AL-INV-9 holds it until every retained tier is verified, and the hot table grows until this clears.

**Triage.**
1. Read the last reconcile run's `archival pass complete` and `drop refused: <reason>` lines.
2. Read the per-tier state in `audit_event_archive_state`.
3. Map the reason to a cause: a failed S3/KMS tier, `not_verified` (redaction reset), `redaction_pending` (go to `IAMAuditLogRedactionPending`), or a repeated `count_mismatch` (late writes).

**Mitigate.** Fix the cause, then run the reconcile job now (`kubectl create job --from=cronjob/iam-audit-log-reconcile …`). Never detach or drop a partition by hand. Full procedure: [RB-2](../runbook.md#rb-2--archival-stalled).

### IAMAuditLogArchiveLagWarning / IAMAuditLogArchiveLagCritical

**Meaning.** The oldest eligible partition has waited more than 1 day (warning) or 3 days (critical) for archival (`iam_audit_log_archive_lag_seconds`). The nightly reconcile run should have archived and dropped it.

**Triage and mitigation.** As for `IAMAuditLogArchiveStalled`. Also check that the reconcile CronJob ran at all (`IAMAuditLogReconcileJobFailed`, `kubectl get jobs`). [RB-2](../runbook.md#rb-2--archival-stalled).

### IAMAuditLogRedactionPending

**Meaning.** `iam_audit_log_redaction_pending_tasks > 0` for 15 minutes: GDPR redaction tasks are stuck pending past `OPS_REDACTION_PENDING_AGE`. This is a **compliance incident**. Archival of any partition the subject touches is refused until they finish (AL-INV-12).

**Triage.** List the pending tasks with their `error` (`audit_redaction_tasks WHERE status = 'pending'`), and check the `redaction-retry` CronJob's runs (`IAMAuditLogMaintenanceJobFailed`).

**Mitigate.** Run `redaction-retry` now. A task that keeps failing points at:
- reconciler-role connectivity, or
- `apply_redaction()` not owned by `audit_reconciler` (RELEASE_CHECKLIST, gap 34).

`iam_audit_log_redaction_tasks_total{status="missed"}` is **not** an alert: archived `security_3y` rows are retained under Object Lock (AL-Q15, option A). Full procedure: [RB-7](../runbook.md#rb-7--redaction-task-stuck-pending).

## Security

### IAMAuditLogCrossTenantAccess

`metric_status: proposed`

**Meaning.** `iam_rls_violations_total{violation_type="cross_tenant_access"}` increased: RLS rejected a row whose tenant did not match the bound `app.tenant_id`. RLS is fail-closed (AL-INV-3), so the failure mode is too few rows, never leakage. `rls_violation_log` is a 1% sample, so any count is significant. Treat it as a possible attack until explained.

**Triage.** Read the latest `rls_violation_log` rows (table, role, `application_name`, query prefix). Correlate them by time with AL-5/AL-6 callers (`forbidden_peer`, an `Idempotency-Key` reused across tenants).

**Mitigate.** [RB-5](../runbook.md#rb-5--suspected-tenant-isolation-issue). Page security if no caller explains it.

### IAMAuditLogMissingGUC

`metric_status: proposed`

**Meaning.** `iam_rls_violations_total{violation_type="missing_or_invalid_guc"}` increased: a query ran without a valid tenant GUC and RLS returned zero rows. This is a bug, not an attack. Some code path is not binding the tenant through pgcommon's transaction-local GUC.

**Triage.** Find the path from `rls_violation_log.query_text`. Check whether it runs outside `RunInTx` on the app pool, or with an empty `requestctx` tenant.

**Mitigate.** Fix the code path. [RB-5](../runbook.md#rb-5--suspected-tenant-isolation-issue).

## Dependencies

### IAMAuditLogDependencyErrors

`metric_status: proposed`

**Meaning.** More than 10% of calls to one dependency ended in `outcome="error"` over 10 minutes (`service:platform_dependency_request_error_ratio:10m`).

**Triage.** `sum by (dependency, operation) (rate(platform_dependency_request_seconds_count{service="iam-audit-log", outcome="error"}[10m]))`.
- `catalog-admin`: the CAT-I2 plans poll. See `IAMAuditLogCatalogPlansStale*` and [RB-8](../runbook.md#rb-8--catalog-plans-poll-failing--stale).
- `s3`: archived reads (503 `dependency_unavailable`), exports or archival. See [RB-2](../runbook.md#rb-2--archival-stalled).
- `sqs` or `codec`: platform-events' own calls (receive, delete, visibility, Glue decode). Check the AWS service health and the IRSA role.

### IAMAuditLogCatalogPlansStaleWarning / IAMAuditLogCatalogPlansStaleCritical

**Meaning.** No successful CAT-I2 plan-map poll for more than 2× (warning) or 10× (critical) `CATALOG_PLANS_POLL_INTERVAL` (`iam_audit_log_catalog_plans_stale_seconds`). The last good map is still served (stale-if-error, AL-D15), so no tenant's window collapses. A plan-window change on Catalog, however, is not propagating.

**Triage.**
1. Check the poll results: `sum by (result) (increase(iam_audit_log_catalog_plans_poll_total{service="iam-audit-log"}[30m]))`.
2. Check Catalog's reachability from a pod in `iam`.
3. A 401 or 403 from Catalog means the `iam-system` identity was rejected, not that Catalog is down.

**Mitigate.** Escalate to Catalog past the critical threshold. [RB-8](../runbook.md#rb-8--catalog-plans-poll-failing--stale).

## Reconciler

### IAMAuditLogReconcileJobFailed

**Meaning.** A `reconcile` run exited non-zero (`kube_job_status_failed`). Possible causes:
- partitions are **blocked** by a pending redaction;
- partitions **stalled**: an unverified tier, or persistent late writes;
- the pass itself failed (DB or S3).

The reconciler is not scraped. The job's exit code is the signal.

**Triage.** `kubectl -n iam logs job/<failed-job>` (structured, IDs only), then continue with [RB-2](../runbook.md#rb-2--archival-stalled) or [RB-7](../runbook.md#rb-7--redaction-task-stuck-pending) depending on the logged reason.

**Mitigate.** Every job is idempotent and safe to re-run (`kubectl create job --from=cronjob/…`). [RB-9](../runbook.md#rb-9--reconciler-cronjob-failed--service-down).

### IAMAuditLogMaintenanceJobFailed

**Meaning.** `redaction-retry`, `redaction-sweep` or `processed-events-prune` exited non-zero.

**Triage.** Read the job's logs.
- `redaction-retry`: at least one pending task is still failing, so continue with `IAMAuditLogRedactionPending`.
- `redaction-sweep`: `sweep_redactions()` failed.
- `processed-events-prune`: the ledger prune failed. Dedup still holds through `uq_audit_events_source_id`.

**Mitigate.** Re-run the job. [RB-9](../runbook.md#rb-9--reconciler-cronjob-failed--service-down).

## SLO burn rates

Fast burn means the 1 h and 5 m error ratios are both above 14.4× the 1% budget (critical, `for: 2m`). Slow burn means the 6 h and 30 m ratios are above 6× (warning, `for: 15m`). The SLIs are defined in [README.md](README.md#slos).

### IAMAuditLogIngestLagSLOFastBurn / IAMAuditLogIngestLagSLOSlowBurn

**Meaning.** More than the budgeted share of rows is persisted later than 50 ms after `occurred_at` (`slo:audit_log_ingest_lag:error_ratio_*`).

**Triage.** As for `IAMAuditLogIngestLagHigh`, plus the backlog (`IAMAuditLogQueueBacklog`) and consumer failures. [RB-1](../runbook.md#rb-1--dlq-has-messages).

### IAMAuditLogPropagationSLOFastBurn / IAMAuditLogPropagationSLOSlowBurn

`metric_status: proposed`

**Meaning.** More than the budgeted share of events is first received later than 5 s after the envelope time (`platform_event_propagation_seconds`). Propagation covers the producer's outbox, SNS and SQS. The persist step is the ingest-lag SLO.

**Triage.** `platform_queue_depth`, the failure ratio and consumer health here. If the queues are empty, the delay is upstream, in the producer's outbox or in SNS: check `platform_outbox_oldest_pending_age` for the producing service.

### IAMAuditLogAvailabilitySLOFastBurn / IAMAuditLogAvailabilitySLOSlowBurn

`metric_status: proposed`

**Meaning.** More than the budgeted share of HTTP requests (query, export, direct-write, AL-7) return 5xx (`slo:audit_log_availability:error_ratio_*`).

**Triage.** As for `IAMAuditLogHighErrorRate`: Postgres and S3 reachability (`dependency_unavailable`), and recent deploys. Direct-write producers retry, so check `IAMAuditLogDirectWriteErrors` as well. [RB-6](../runbook.md#rb-6--direct-write-producer-failing).
