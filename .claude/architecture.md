# Architecture

Ports and adapters (LLD §3), with dependency rules in `.go-arch-lint.yml` (`depOnAnyVendor: false`) plus the grep gates in `.github/scripts/arch-lint.sh`.

## Composition roots

| Binary | Runs as | Hosts |
|---|---|---|
| `cmd/server` (Deployment, 3 replicas) | `audit_app` (INSERT+SELECT, RLS-bound); migrations use the migrator DSN at startup | Gin API (`APP_PORT` 8080), `/metrics` on `METRICS_PORT` 9090, and the background workers below |
| `cmd/reconciler` (CronJobs, `--job=`) | `audit_reconciler` (BYPASSRLS) | one job per run, guarded by a lease row (`reconciler_job_lease`, migration 000011): a run that finds it held exits 0 without running; exits non-zero when blocked or stalled |

Neither root ever opens the other's role pool (rule 5): each has its own Helm Secret, and each checks its role at startup.

## `cmd/server` background workers

| Worker | Code | Purpose |
|---|---|---|
| Consumer fleet | `inbound/consumer` (`Fleet`, `Handler`) | One platform-events `NewSQSConsumer` per queue, with the Glue codec and `WithDLQForwarding` (malformed/undecodable → the queue's DLQ, D-22). The dead-letter observer fires at `SQS_MAX_RECEIVE_COUNT − 1` and returns an error so SQS redrives. `Stop` stops all consumers concurrently. Everything converges on `IngestService.IngestBus`. |
| Plan poller | `service/plan_poller.go`, `outbound/catalog` | CAT-I2 `GET /api/v1/internal/plans` every `CATALOG_PLANS_POLL_INTERVAL`. The map is swapped only when `record_versions` changes, and a failed poll keeps the last good map (stale-if-error). |
| Export worker | `service/export_service.go` | Claims jobs through `claim_export_job()`, with a lease plus heartbeat. It writes a gzipped JSONL object to S3 with SSE-KMS. |
| Ops monitor | `service/ops_monitor.go` | Every `OPS_STATS_INTERVAL`: `audit_ops_stats()` plus DLQ depth feed the alerting gauges (D-21). |

The poller, export worker and ops monitor run under `cmd/server/supervise.go`: a panic is recovered, logged with its stack, and the loop restarts after 5 s.

## Reconciler jobs (`cmd/reconciler/jobs`)

| Job | File | Schedule (Helm) |
|---|---|---|
| `reconcile` | `reconcile.go`: partitions → re-open → archive → verify → drop (`service/archive_service.go`) | `0 2 * * *` |
| `processed-events-prune` | `reconcile.go` | `0 3 * * *` |
| `redaction-retry` | `redaction_retry.go` | `*/15 * * * *` |
| `redaction-sweep` | `redaction_retry.go` | `30 3 * * *` |

## Core

- **`core/domain`** (stdlib only):
  - the entry and actor models;
  - `taxonomy.go` (entry_type → tier; the bus map);
  - `bus.go` (normalization, D-8/D-9); `query.go` (filter, cursor, window clamp);
  - `archive.go` / `lifecycle.go` (keys, eligibility, retain-until); `redaction.go`; `ops.go`; `errors.go` (§17).
- **`core/port`:** the interfaces only, with no vendor imports. `Logger` / `SlogStyleLogger` take an injected `TraceIDFunc`, and `Tracer` is also a port.
- **`core/service`:** `IngestService` (+bus, +redaction), `QueryService` (archived reads bounded by `ARCHIVE_READ_CONCURRENCY` slots), `ExportService`, `ArchiveService`, `PartitionService`, `PlanPoller`, `OpsMonitor`.

## Adapters

| Adapter | Responsibility |
|---|---|
| `inbound/http` | Router, middleware (`RequireAuditReader`, `RequireSystemRole`, `RequireIdempotencyKey`, per-tenant rate limiters), handlers for AL-1..AL-7. Errors are RFC 9457 problem details (`application/problem+json`) rendered by platform-gincommon v2 via `AbortWithProblem`; domain types (`ProblemResponse = problem.Problem`) registered at package init from the §17 taxonomy. |
| `inbound/consumer` | The platform-events consumer fleet and envelope → `domain.BusEvent` |
| `outbound/postgres` | All SQL, via pgcommon (`withPool`, `TxRunner`). Repositories: audit, query, export, plan-window, partition, archive, redaction, ops, job lease; plus migrations. `wrapConnErr` classifies with `pgcommon.Is*` (closed pool, 08/53/57/58 → 503) |
| `outbound/s3` | Archive/export store: SSE-KMS, Object Lock, verify, presign |
| `outbound/glue` | Decode-only Glue schema-registry codec; validation errors are sanitized to paths and keywords |
| `outbound/catalog` | CAT-I2 client, sent as `iam-system` |
| `outbound/metrics` | Every Prometheus collector, built in `buildCollectors`: `gincommon.NewShared*Vec` for registry names (Tier 1/2), gincommon's validating constructors for `iam_audit_log_*` (Tier 3), each wrapped in `must(...)` |
| `outbound/telemetry` | Thin wrappers over gincommon (`NewTracer` → `NewSpanTracer`, `TraceID` → `SpanTraceID`, `MetricsHandler`), plus `HTTPErrorLog`, `QuietGin` |

## Transport and library confinement

- **SQS:** a raw client exists only in `cmd/server/main.go`, for the DLQ depth gauge. Consumption is entirely platform-events; the one SQS writer is its DLQ publisher, wired there for `WithDLQForwarding` (D-22).
- **S3:** only `outbound/s3` plus client construction in the roots. **Glue:** only `outbound/glue` plus `cmd/server`.
- **pgx:** only `outbound/postgres`; pools only through `pgcommon.NewPool`.
- **OTel/promhttp:** imported nowhere; gincommon's tracing and metrics API only.
- **Logging:** only gincommon's `logger.NewLogger`.

See the table in [CLAUDE.md](CLAUDE.md) for the gate that enforces each rule.
