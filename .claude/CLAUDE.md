# CLAUDE.md

Guidance for Claude Code working in **`iam-audit-log`**: the Tender Management SaaS platform's compliance system of record. It is a Go service in the IAM subsystem, module `github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log`.

- **Spec:** `docs/lld/iam-lld-audit-log-service.md` (revision table at the top; latest rev 0.24).
- **Build trace, decisions and gaps:** `docs/implementation/BUILD_PLAN.md`.
- Where the LLD and a BUILD_PLAN decision (D-n) disagree, the decision is the implemented behavior, and the LLD note it needs is recorded in BUILD_PLAN §C.

Topic files in this directory: [architecture.md](architecture.md), [database-schema.md](database-schema.md), [api-caching-events.md](api-caching-events.md), [request-flows.md](request-flows.md), [operations.md](operations.md).

## What this service is

A single append-only, tenant-scoped store answering *who did what, to what, when, from where* across every IAM and domain service.

- **Ingests** 11 SNS topics via SQS (one queue each, catch-all, at-least-once, deduplicated). It also takes non-bus entries via mesh-only direct-write: AL-5/AL-6 `POST /api/v1/internal/audit-entries[:batch]`.
- **Serves** tenant-admin query and export (AL-1..AL-4) within a plan-gated window, plus a mesh-only provenance read (AL-7).
- **Archives** months past the hot window to S3 (SSE-KMS, Object Lock COMPLIANCE, per-tenant objects). It drops a partition only when it is provably archived, and it applies GDPR redaction to `security_3y` rows.
- **Publishes nothing** (AL-INV-10): no SNS publisher, no outbox.

**Status:** build phases 0–8 are complete and committed. Later work, including the platform-library confinement of gaps 43–45, may be uncommitted on the current branch. See `git log` / `git status` rather than assuming.

## Binding rules

Invariants (LLD §2.4; the test map is BUILD_PLAN §B):
- **AL-INV-1** — `audit_app` has exactly INSERT+SELECT on `audit_events`; a trigger rejects UPDATE/DELETE from anyone but `audit_reconciler`.
- **AL-INV-2** — one write path, `AuditWriter.Append`, for the bus and direct-write.
- **AL-INV-3** — tenant isolation by FORCE RLS with a fail-closed `app.tenant_id` GUC. It is bound only transaction-locally, and the app pool forces PgBouncer mode.
- **AL-INV-4** — dedup: the `processed_events` ledger plus the `uq_audit_events_source_id` backstop.
- **AL-INV-5** — provenance columns are NOT NULL.
- **AL-INV-6** — the retention tier is derived from `entry_type`; durations are compiled constants.
- **AL-INV-7** — redaction never touches `compliance_7y`.
- **AL-INV-8** — the plan window is a service-layer clamp and never shortens storage.
- **AL-INV-9** — a partition is dropped only when every retained tier is verified (`audit_drop_partition()`).
- **AL-INV-10** — no publisher or outbox.
- **AL-INV-11** — a caller-supplied tier is ignored.
- **AL-INV-12** — redact on consume, and refuse to archive a partition with a pending redaction.

Platform-library confinement (each rule is a CI gate in `make ci`):

| Concern | Only through | Enforced by |
|---|---|---|
| Logs, metrics, traces | platform-gincommon. `internal/adapter/outbound/telemetry` is the one OTel/promhttp seam, always against gincommon's provider and registry (gap 43). | `.github/scripts/check-observability-confinement.sh` |
| DB connection, config, operations | platform-pgcommon: `NewPool`, `ConfigFromEnv`, `RunInTx*`, `Is*`, `pkg/migrate` (gap 44) | `arch-lint.sh` database invariant |
| Events, SQS config, dedup | platform-events: `NewSQSConsumer`, `config.LoadSQS`; dedup keyed on `Envelope.ID` (gap 45) | `check-forbidden-events-bypass.sh` |

Other gates: `.go-arch-lint.yml` (the core is vendor-free), `check-grants.sh`, `check-asyncapi-receive-only.sh`, `check-metric-naming.sh`, and the contract tests in `test/unit`.

## Common commands

```bash
make setup                      # .env from .env-example + git hooks
make docker-up / docker-down    # local Postgres (5548) + floci (4573)
make run                        # cmd/server (sources .env)
make run-reconciler JOB=<name>  # reconcile | redaction-retry | redaction-sweep | processed-events-prune
make test-unit                  # no Docker
make test-postgres              # testcontainers PG15: RLS, grants, partitions, repositories
make test-integration           # floci SQS/Glue/S3 + PG: consumer fleet, reconciler
make test-e2e                   # HTTP e2e, real binaries
make test-ci                    # unit + postgres + integration, -race, merged coverage (gate 95%)
make lint / arch-lint / invariant-lint / fmt-check / vet
make swag / swag-check          # docs/swagger
make ci                         # tidy fmt-check vet lint arch-lint invariant-lint test-ci build
```

`make test-ci` plus `make test-e2e` together take more than 10 minutes, so run them in the background.

## Layout

```
cmd/server, cmd/reconciler (+ jobs/)  composition roots (one DB role each)
internal/core/{domain,port,service}   hexagonal core; domain is stdlib-only, port has no vendor deps
internal/adapter/inbound/{http,consumer}
internal/adapter/outbound/{postgres,s3,glue,catalog,metrics,telemetry}
internal/config                       env → typed config (library-owned PG_*/SQS_*/OTEL_* excluded)
api/                                  asyncapi.yaml (receive-only) + embedded specs
deploy/{helm,iam,monitoring}          chart, IRSA policy, PrometheusRule mirror
docs/{lld,implementation,runbook.md,swagger}
test/{unit,postgres,integration,e2e,fixtures,dbseed}
```

## Conventions

- **Layers:** `.go-arch-lint.yml`. Adapters never import each other, and only `cmd/*` wires them together.
- **Errors:** the §17 codes live in `internal/core/domain/errors.go`, rendered by `inbound/http/errors.go`. A new code needs tests in both `domain/errors_test.go` and `test/unit/domain_contract_test.go`.
- **Logging:** IDs only, never `metadata` or payload. `TestLogHygiene_NoPayloadFields` enforces a denylist.
- **DB roles:** `cmd/server` uses only `audit_app` (plus the migrator DSN for startup migrations); `cmd/reconciler` uses only `audit_reconciler`.
- **Definer functions:** anything a role can't do by grant goes through a narrow SECURITY DEFINER function, e.g. partition DDL, the export claim, redaction, the drop/re-open gate and ops stats. `app_tenant_id` / RLS helpers are separate.
- **jsonb binding:** bind as `string`, never `[]byte` (PgBouncer mode, gap 28).
- **Decisions:** D-1..D-21 live in BUILD_PLAN §C, and each has a date and "user" when the user chose it. Don't invent behavior; if the spec is silent, raise a gap and ask.

### How to add…

- **An entry_type:** add a row in `domain/taxonomy.go` (tier included), the matching message in `api/asyncapi.yaml`, and an LLD §7.1 row. `test/unit/taxonomy_contract_test.go` parses the LLD.
- **A metric** (Enterprise Platform Observability Standard, LLD §11):
  - Pick the tier by the decision tree. A new `platform_*` or `iam_*` name needs registry ratification first (standard rules 11–12), so default to Tier-3 `iam_audit_log_*`.
  - Register it in `internal/adapter/outbound/metrics/business.go` (Tier-1 `pLabels`, Tier-2/3 `sLabels`; never set `service`/`domain`/`environment` at a call site). Bound every label value; never use tenant, user, request or event ids as labels.
  - Add it to `deploy/monitoring/metric-registry.yaml` (shared names also to `registry.go`) and give it an LLD §11 row. `TestMetricRegistry_*` and `TestMetrics_LLDTier3Registered` check this.
  - To rename, keep the old name as `status: deprecated` with `replaced_by`/`sunset`, emit both, and move every rule, SLO, dashboard and HPA reference in the same change; CI rejects references to deprecated names.
- **An alert, recording rule or SLO:** edit `deploy/monitoring/{app-alerts,recording-rules,slo-rules}.yml`, then run `python3 scripts/gen-prometheusrule.py` to regenerate the Helm `PrometheusRule` (a test keeps them identical).
- **A migration:** use the next `NNNNNN_name.{up,down}.sql` under `internal/adapter/outbound/postgres/migrations/`. Bump the version in `test/postgres/migrations_test.go`, and put grants in guarded `DO $$ … $$` blocks.
- **A reconciler job:** write the function in `cmd/reconciler/jobs`, register it in `Registry()`, and add a Helm `cronjobs` entry. Update the registry test and `test/unit/deploy_contract_test.go`.

## Testing notes

- **Shared floci:** the integration package shares one floci per run (`test/integration/harness_test.go`). A test that asserts on a queue or DLQ must purge it first and filter to its own event ids.
- **Postgres tests** seed as superuser through `test/dbseed`, which wraps `pgcommon`.
- **Archived objects:** manifest seeds representing archived data need `sealed=true`.
- **Coverage:** merged coverage is about 96.5% against a 95% gate.
