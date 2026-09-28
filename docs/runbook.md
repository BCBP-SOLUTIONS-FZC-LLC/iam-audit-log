# Operator Runbook — iam-audit-log

These are the full procedures for the LLD §24 runbooks, with decisions from
`docs/implementation/BUILD_PLAN.md` §C applied. This service is the platform's
compliance system of record. Its two standing rules:

- **Never purge a DLQ.**
- **Never delete or rewrite an audit row by hand.** The only mutation paths are
  `apply_redaction()`, `sweep_redactions()` and the reconciler's partition
  functions.

Alert, recording and SLO rules live in `deploy/helm/templates/prometheusrule.yaml`
(generated from `deploy/monitoring/{app-alerts,recording-rules,slo-rules}.yml`).
The Critical archival, redaction and DLQ gauges are published by `cmd/server` from
DB/SQS state (D-21), so they fire even though the reconciler CronJob is not scraped.

Metric names follow the Enterprise Platform Observability Standard (LLD §11 rev
0.26; inventory `deploy/monitoring/metric-registry.yaml`). Tier-1 `platform_*`
series carry `{domain="iam", service="audit-log"}`, and `iam_*` series carry
`{service="audit-log"}`. Three old names are still emitted but deprecated; use
their replacements in queries:

| Deprecated | Use |
|---|---|
| `iam_audit_log_dlq_messages_total{queue="<q>-dlq"}` | `platform_dlq_depth{queue="<q>"}` |
| `iam_audit_log_default_partition_rows_total` | `iam_audit_log_default_partition_rows` |
| `iam_audit_log_archive_stalled` | `iam_audit_log_archive_stalled_partitions` |

Dashboard: `deploy/monitoring/dashboard-audit-log.json` (Grafana uid `iam-audit-log`).

Conventions used below:

```bash
NS=iam
REL=iam-audit-log                                   # Helm release / fullname
# Read-only SQL as admin_readonly (BYPASSRLS; every session is itself audited).
# Use the reconciler DSN only where a step says so.
PSQL="psql $ADMIN_READONLY_DATABASE_URL"
```

---

## RB-1 — DLQ has messages

**Alerts:** `IAMAuditLogDLQNotEmpty` (`platform_dlq_depth{queue} > 0`, plus the CloudWatch alarm at 0) and `IAMAuditLogDeadLettered` (`increase(platform_dlq_messages_total{queue,reason}[15m]) > 0`). Leading indicators: `IAMAuditLogMessageFailures` (the failure ratio > 5%), `IAMAuditLogRetrySpike` (`platform_retry_total` by reason), `IAMAuditLogQueueBacklog` (`platform_queue_depth` > 1000), and the ingest-lag and propagation SLO burn alerts.
**Severity:** a compliance incident. Every message is a potentially lost audit record (AL-EVT-4).

**Triage**
0. Find the cause class from the metrics first:
   ```promql
   service:platform_messages_failed:rate5m{service="audit-log"}      # by reason: dependency_unavailable = Postgres, invalid_event = producer shape
   max by (queue) (platform_queue_depth{service="audit-log"})         # backlog per queue
   ```
1. Read the messages without deleting them. Pass `--visibility-timeout 0` so they
   stay available:
   ```bash
   aws sqs receive-message --queue-url "$DLQ_URL" --max-number-of-messages 10 \
     --visibility-timeout 0 --attribute-names All --message-attribute-names All
   ```
2. Classify each message from the server logs (`kubectl -n $NS logs deploy/$REL | grep <message_id>`):
   - **Schema/decode failure** (`glue codec: payload fails schema version …`). The error lists instance paths and keywords only, never values. Either the producer shipped a payload its registered schema rejects, or the schema version cannot be resolved.
   - **Poison payload**, e.g. an unstorable `tenant_id`.
   - **Downstream outage** that exhausted `maxReceiveCount` (5), e.g. Postgres unavailable during the retries.
3. Check whether the event is already stored. A replay is always safe (AL-INV-4):
   ```sql
   SELECT id, entry_type, recorded_at FROM audit_events WHERE source_event_id = '<envelope id>';
   ```

**Resolve**
- **Outage:** once the dependency is healthy, redrive:
  `aws sqs start-message-move-task --source-arn <dlq-arn>`.
- **Schema/poison:** fix at the producer, or register the missing schema version, then redrive. Never edit the message body.

**Verify:** the DLQ gauge returns to 0 within `OPS_STATS_INTERVAL`, and the event's `source_event_id` is present in `audit_events`.

---

## RB-2 — Archival stalled

**Alert:** `IAMAuditLogArchiveStalled` (`iam_audit_log_archive_stalled_partitions > 0`), or `IAMAuditLogArchiveLag{Warning,Critical}` (> 1 d / > 3 d).
**Meaning:** a partition is past archival eligibility (+48 h grace) and still attached. AL-INV-9 is holding it, and the hot table keeps growing until this clears.

**Triage**
1. Check the last reconcile run (also RB-9):
   ```bash
   kubectl -n $NS get jobs -l app.kubernetes.io/component=cronjob-reconcile --sort-by=.status.startTime
   kubectl -n $NS logs job/<latest>
   ```
   Look for `archival pass complete` (with dropped / blocked / stalled counts) and any `… failed — partition kept` lines.
2. Check the per-tier state of the stuck months:
   ```sql
   SELECT partition_name, retention_tier, status, row_count, object_count, archived_at, verified_at, error
     FROM audit_event_archive_state
    WHERE status NOT IN ('dropped', 'expired')
    ORDER BY partition_name, retention_tier;
   ```
3. Map the drop-gate reason (logged as `drop refused: <reason>`) to a cause:

| Reason / state | Cause | Action |
|---|---|---|
| tier `failed` with `error` | S3/KMS unavailable, bucket or Object Lock misconfiguration, checksum mismatch on verify | Fix the dependency. The next run re-archives the unsealed parts as new object versions (D-20). |
| `not_verified` | A redaction rewrote a row after upload, so the tier was reset to `pending` (stale-PII guard) | None. The next run re-archives. |
| `redaction_pending` | A pending redaction task touches the month (AL-INV-12) | Go to RB-7. |
| `count_mismatch` (repeated) | Late rows keep arriving for the month | Find the producer: `SELECT source_service, count(*) FROM audit_events WHERE occurred_at >= '<month>' AND occurred_at < '<month>'::date + interval '1 month' AND recorded_at > now() - interval '1 day' GROUP BY 1;` |

**Resolve:** fix the cause, then run the job now instead of waiting for 02:00:
```bash
kubectl -n $NS create job --from=cronjob/$REL-reconcile $REL-reconcile-manual-$(date +%s)
```
Never detach or drop a partition by hand. `audit_drop_partition()` is the only drop path, and it re-checks every condition under lock.

**Verify:** `iam_audit_log_archive_stalled_partitions` is 0, archive lag is back under 1 d, and the state rows read `dropped` (retained tiers) / `expired` (`access_90d`).

---

## RB-3 — Rows in `audit_events_default`

**Alert:** `IAMAuditLogDefaultPartitionRows` (`iam_audit_log_default_partition_rows > 0`).

**Triage**
```sql
SELECT to_char(occurred_at AT TIME ZONE 'UTC', 'YYYY_MM') AS month, source_service, count(*)
  FROM audit_events WHERE tableoid = 'audit_events_default'::regclass
 GROUP BY 1, 2 ORDER BY 1;
SELECT partition_name, status FROM audit_event_archive_state
 WHERE partition_name IN (SELECT 'audit_events_' || m FROM (VALUES ('<month>')) v(m));
```

**Resolve by case**
- **The month was dropped** (state `dropped`): this is **automatic** (D-19). The next reconcile run calls `audit_reopen_partition()`. It recreates the month, moves its rows in, and resets the retained tiers to pending, and the run then re-archives (new parts after the sealed ones) and re-drops. To speed it up, run the reconcile job manually (RB-2).
- **The month was never partitioned** (a bad producer clock, far past or future): manual.
  1. Fix the producer's clock and file against the source service.
  2. If the month is legitimately inside the pre-create window but was skipped (`partition creation blocked by rows in audit_events_default`), ask the DB owner (as `audit_migrator`) to re-home the rows the same way `audit_reopen_partition()` does: detach DEFAULT, create the month, re-insert through the parent, drop the old DEFAULT. Never `DELETE` the rows.

**Verify:** the gauge returns to 0.

---

## RB-4 — "I can't see audit older than X"

Expected if X is beyond the tenant's plan window (HLD §6.6; AL-INV-8).
```sql
SELECT plan_code, query_window_days, last_event_at FROM tenant_plan_window WHERE tenant_id = '<tenant>';
```
- The window resolves from the CAT-I2 map for `plan_code`, then the row's stored value, then `AUDIT_DEFAULT_QUERY_WINDOW_DAYS` only when there is no row (D-13).
- If the plan entitles more, check for a missed `TenantPlanChanged` (the projection is stale), or check that the Catalog poll is healthy (RB-8).
- The data is stored regardless. Compliance and legal can retrieve it through AL-7 (no clamp). An AL-7 archived range over the sync bounds returns `422 range_too_large`; narrow `from`/`to` (D-14).

---

## RB-5 — Suspected tenant-isolation issue

**Alerts:** `IAMAuditLogCrossTenantAccess` (Critical), `IAMAuditLogMissingGUC` (Warning).

RLS is fail-closed (AL-INV-3), so the failure mode is *too few* rows, never leakage.
```sql
SELECT occurred_at, table_name, violation_type, session_role, application_name, left(query_text, 200)
  FROM rls_violation_log ORDER BY occurred_at DESC LIMIT 50;   -- a 1% sample
```
- **`cross_tenant_access`:** a write or read carried a tenant that did not match the bound GUC. Correlate by time with AL-5/AL-6 callers (`forbidden_peer`, `400` on a key reused across tenants) and treat it as a possible attack until explained.
- **`missing_or_invalid_guc`:** a code path queried without binding `app.tenant_id`. Find it from `query_text`; it is a bug, not an attack.

---

## RB-6 — Direct-write producer failing

**Alert:** `IAMAuditLogDirectWriteErrors` (`directwrite_requests_total{result="error"}` > 5%).
1. Break it down by `source_service`:
   `sum by (source_service, result) (rate(iam_audit_log_directwrite_requests_total[10m]))`.
2. Results:
   - **`rejected`** (4xx, the producer's fault): `unknown_entry_type` (direct-write accepts only the D-6 types), `invalid_actor`, `metadata_too_large`, `forbidden_peer` (a mesh identity other than `iam-system`).
   - **`error`** (5xx): Postgres unavailable, so check pool and DB health.
3. The producer must keep committing its own business writes (AUDIT-CALLER-1) and retry with the **same** `Idempotency-Key`. A replay returns `200` with the existing entry.

---

## RB-7 — Redaction task stuck pending

**Alert:** `IAMAuditLogRedactionPending` (`iam_audit_log_redaction_pending_tasks > 0` for 15 m).
**Severity:** a compliance incident, the same posture as RB-1. Archival of any partition the subject touches is refused while the task is pending (AL-INV-12).

```sql
SELECT id, tenant_id, trigger_source_event_id, requested_at, error
  FROM audit_redaction_tasks WHERE status = 'pending' ORDER BY requested_at;
```

**Resolve**
1. The immediate apply after `UserDeleted` failed (logged as `immediate redaction failed — task left pending`). The `redaction-retry` CronJob re-applies it every 15 min. Check its runs (RB-9). To run it now:
   `kubectl -n $NS create job --from=cronjob/$REL-redaction-retry $REL-redaction-retry-manual-$(date +%s)`
2. `apply_redaction()` is idempotent. A task that keeps failing points at reconciler-role connectivity, or at a broken definer ownership (`apply_redaction` must be owned by `audit_reconciler`; see RELEASE_CHECKLIST, gap 34).

**Not an alert: `missed`.** A task that ends `missed` redacted the hot rows, and the subject also has archived `security_3y` rows. Those rows are retained under Object Lock (AL-Q15, Option A, LLD rev 0.22). This is routine for any long-lived user. No action is needed unless Compliance/Legal mandates Option B.

**Late arrivals (D-18):** rows about an erased subject that arrive after the task are redacted **on ingest** (`_redacted_on_ingest`). The daily `redaction-sweep` fixes any that slipped through. A non-zero sweep count is logged at warn, and means the ingest check was bypassed, so investigate.

---

## RB-8 — Catalog plans poll failing / stale

**Alerts:** `IAMAuditLogCatalogPlansStale{Warning,Critical}` (> 2× / > 10× `CATALOG_PLANS_POLL_INTERVAL`); `IAMAuditLogDependencyErrors` for `dependency="catalog-admin"` (`platform_dependency_request_seconds` error/timeout ratio > 10%); `iam_audit_log_catalog_plans_poll_total{result="error"|"timeout"}` rising. The same dependency alert with `dependency="s3"` points at archived reads, exports or archival (RB-2).

The service keeps serving the last good plan map (stale-if-error, AL-D15). No tenant's window collapses to the default.
1. Check reachability and health of Catalog: `GET {CATALOG_BASE_URL}/api/v1/internal/plans` from a pod in `iam`. Catalog admits same-namespace callers only (AL-Q17).
2. Check the `iam-system` headers are accepted: a 401/403 from Catalog is an identity problem, not an outage.
3. Escalate to Catalog past the Critical threshold, because a plan-window change is not propagating.

---

## RB-9 — Reconciler CronJob failed / service down

**Alerts:** `IAMAuditLogReconcileJobFailed` (Critical), `IAMAuditLogMaintenanceJobFailed` (Warning), `IAMAuditLogDown`.

| CronJob | Schedule | A non-zero exit means |
|---|---|---|
| `reconcile` | 02:00 daily | partitions **blocked** (pending redaction), **stalled** (a failed or unverified tier, persistent late writes), or the pass failed (DB/S3) |
| `redaction-retry` | every 15 min | at least one pending task still failing |
| `redaction-sweep` | 03:30 daily | `sweep_redactions()` failed |
| `processed-events-prune` | 03:00 daily | the ledger prune failed (dedup still holds via `uq_audit_events_source_id`) |

```bash
kubectl -n $NS get cronjobs -l app.kubernetes.io/instance=$REL
kubectl -n $NS logs job/<failed-job>          # structured; IDs only, never payload
```
Every job is idempotent and safe to re-run. Use `kubectl create job --from=cronjob/…` as in RB-2. For `reconcile`, continue with RB-2 or RB-7 depending on the logged reason. If the whole service is down, the audit trail is not lost: events wait in SQS (14 d including the DLQ), and direct-write producers retry.

---

## RB-10 — Malformed envelopes

**Alert:** `IAMAuditLogMalformedEnvelopes` (`events_consumed_total{status="malformed"}` increase).
**Known gap (D-3):** platform-events v1.4.0 **deletes** a message whose body is not a valid envelope. It is not retried and not DLQ'd, so the message is gone once this fires.
1. Find the logs: `kubectl -n $NS logs deploy/$REL | grep "failed to unmarshal message body"`. They carry the queue and message id. (The library also logs the raw body, BUILD_PLAN gap 42, so handle these logs as sensitive.)
2. Identify the producer from the queue (`<topic>` → `*-audit-q`), and fix its envelope serialisation.
3. Ask the producer to re-emit the affected events from its outbox. With the same envelope id, a re-emit is deduplicated if the event somehow did land.
4. Track the upstream platform-events fix (leave malformed messages for the DLQ).
