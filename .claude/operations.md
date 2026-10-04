# Operations

## Configuration

- **Service env:** see `.env-example` (the documented defaults) and `internal/config/config.go`. Helm splits it into `env` (shared), `serverEnv` and `reconcilerEnv`, and each root gets its own Secret holding only its own DSN.
- **Library-owned env,** never read by this repo:
  - `PG_*` pool settings → `pgcommon.ConfigFromEnv`. The service injects only the DSN: `DATABASE_URL`, `RECONCILER_DATABASE_URL`, or `MIGRATION_DATABASE_URL` for migrations.
  - `SQS_*` (visibility timeout, max receive = the queues' redrive count, concurrency, batch size, long-poll) → platform-events `config.LoadSQS`.
  - `OTEL_*` → gincommon `InitTracingWithConfig` (endpoint, insecure, sampler ratio, baggage, remote-parent sampling; `OTEL_EXPORTER_OTLP_ENDPOINT=none` disables export). `LOG_LEVEL` / `LOG_SAMPLING` → the gincommon logger.
  - Not read: `APP_NAME`, `OTEL_SERVICE_NAME` (the name is `SERVICE_NAME`, passed in code) and `PG_SSLMODE` / `PG_HOST` / `PG_USER` (each pool gets an explicit DSN).
- **Validation:** config is checked at startup. Invalid values are logged through the gincommon logger and the process exits 1.

## CronJobs (`deploy/helm/values.yaml` → `cronjobs`)

| Job | Schedule | Key env |
|---|---|---|
| `reconcile` | `0 2 * * *` | `AUDIT_HOT_WINDOW_DAYS`, `AUDIT_PRECREATE_MONTHS`, `AUDIT_WRITABLE_TRAILING_MONTHS`, `ARCHIVE_PART_MAX_ROWS`, `RECONCILER_TIMEOUT` |
| `processed-events-prune` | `0 3 * * *` | `PROCESSED_EVENTS_TTL_DAYS` (8), `PROCESSED_EVENTS_PRUNE_BATCH` |
| `redaction-retry` | `*/15 * * * *` | `REDACTION_RETRY_MIN_AGE`, `REDACTION_RETRY_BATCH` |
| `redaction-sweep` | `30 3 * * *` | `REDACTION_SWEEP_WINDOW` (2160h) |

All run with `concurrencyPolicy: Forbid`, and each run also claims a lease row, so a manually created Job (`kubectl create job --from=…`) cannot overlap a scheduled one; the loser exits 0. A blocked or stalled `reconcile` exits non-zero; see RB-9.

## Metrics and alerts

- **Standard:** the Enterprise Platform Observability Standard (LLD §11 rev 0.26, gap 46). Every series carries `{domain="iam", service="iam-audit-log", environment}`, injected by gincommon. Tier-1 `platform_*` (libraries; five built by the service from the registry), Tier-2 `iam_rls_violations_total` (registry, proposed), Tier-3 `iam_audit_log_*`. Docs: `docs/observability/` (README, generated `metric-registry.md`, `migration.md`, per-alert `runbooks.md`). The service inventory is `deploy/monitoring/metric-registry.yaml`; the shared-name ledger is `internal/adapter/outbound/metrics/registry.go`. `make metrics-lint` / `make metrics-inventory`.
- **Collectors:** all are in `internal/adapter/outbound/metrics/business.go`.
- **Ops gauges (D-21):** `cmd/server` publishes these from `audit_ops_stats()`, SQS and `audit_rls_violation_counts()`:
  - `platform_queue_depth`, `platform_dlq_depth` (per inbound queue);
  - `iam_audit_log_archive_stalled_partitions`, `archive_lag_seconds`, `default_partition_rows`, `redaction_pending_tasks`;
  - `iam_rls_violations_total`.
- **Removed names:** the pre-standard names and the three gap-46 twins are gone (see `docs/observability/migration.md`); `metricslint refs` rejects them in `deploy/` and the operator docs.
- **Docs and lint:** `docs/observability/` (README, generated `metric-registry.md`, `migration.md`, per-alert `runbooks.md`). `make metrics-lint` runs gincommon's `metricslint`; `make metrics-inventory` regenerates the inventory.
- **Reconciler-only counters** stay internal (no scrape endpoint on a CronJob, gap 40); the CronJob-failure alert covers them.
- **Rules:** `deploy/monitoring/{app-alerts,recording-rules,slo-rules}.yml`. The Helm `PrometheusRule` is generated from them (`make prometheusrule`). Dashboard `dashboard-audit-log.json`; HPA adapter rules `prometheus-adapter-rule.yaml`. See `deploy/monitoring/README.md`.
- **Runbook:** per-alert triage in `docs/observability/runbooks.md`; every alert's `runbook_url` is an absolute link checked by `TestAlerts_RunbookURLsResolve`. Full procedures in `docs/runbook.md`, RB-1..RB-10:

  | Runbook | Covers |
  |---|---|
  | RB-1 | DLQ |
  | RB-2 | archival stalled |
  | RB-3 | DEFAULT partition rows |
  | RB-4 | the window complaint |
  | RB-5 | isolation |
  | RB-6 | direct-write failures |
  | RB-7 | redaction pending |
  | RB-8 | Catalog poll |
  | RB-9 | CronJob failed / service down |
  | RB-10 | malformed envelopes |

## Release

Complete `docs/implementation/RELEASE_CHECKLIST.md` before the first AWS deploy:
- the Terraform role membership `audit_migrator` → `audit_reconciler` (gap 34);
- no default Object Lock retention on the bucket, and an `exports/` lifecycle rule (gap 30);
- the `iam` namespace (AL-Q17);
- CronJob-failure alerting;
- the IRSA policy's KMS statement names the key ARN, and the role may `SendMessage` to the audit DLQs (D-22);
- Istio installed and the `iam` namespace meshed; `mesh.authorizationPolicy.internalCallerPrincipals` and `networkPolicy.internalCallers` list the real AL-5/AL-6/AL-7 callers (D-23).

## Local development

- `make setup && make docker-up` starts Postgres on `localhost:5548` and floci on `localhost:4573` (`scripts/init-floci.sh` creates the 11 queues plus DLQs, the Object-Lock bucket and the Glue registries). Then `make run`, and `make run-reconciler JOB=reconcile`.
- Ports: API `:8080`, metrics `:9090`.
- floci 2.1.0-compat supports Object Lock and echoes it back, but it refuses to overwrite a locked key where S3 would version it (gap 16). Pinned by `TestReconciler_FlociObjectLockSupport`.

## Troubleshooting

| Symptom | Look at |
|---|---|
| Server exits at startup: migration owner error | Gap 34, the role membership |
| `permission denied` (42501) as `audit_app` | Never add a grant. Use a definer function, or avoid `ON CONFLICT (col)` / `RETURNING` on INSERT-only tables. |
| An integration test sees extra DLQ messages | The floci is shared, so purge and filter your own queues |
| Archived rows missing from AL-1 | The manifest row must be `sealed` (D-20) |
| No `trace_id` in logs | Use the `*Context` logging variants; tracing must be initialized by gincommon |
| `403 RBAC: access denied` from the sidecar on `/api/v1/internal/*` | The caller's SPIFFE id is missing from `mesh.authorizationPolicy.internalCallerPrincipals` |
| `postgres dsn: port "5432/tcp" not found` / `init-floci.sh did not finish` in tests | A saturated Docker host; the fixtures retry, otherwise rerun when it is quieter |
| `coverage-gate.sh`: malformed import path in `coverage.out` | Two `make test-ci` runs overlapped in one working tree; rerun one at a time |
