# Changelog

All notable changes to this service. Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versioning: see `VERSIONING.md`.

## [Unreleased]

### Added
- **Phase 3 — bus consumer fleet (LLD §7):**
  - One platform-events consumer per inbound queue.
  - A decode-only Glue codec that validates every payload against the
    producer's registered schema version (AL-D12, §7.3.1).
  - `BuildBusEntry` with the D-8 actor/target rules and the D-9
    truncation marker.
  - Unknown types persisted as `<domain>.unknown` (AL-EVT-4).
  - `tenant_plan_window` projection with a recency guard.
  - Workflow wire-type aliases (D-7).
  - AsyncAPI now lists all 65 consumed messages with their classification.
  - floci + Postgres integration tests covering the whole pipeline,
    including Glue decode and DLQ redrive.
- **Phase 2 — ingest core + direct-write (LLD §5.4, §7.1, §10.3, §17):**
  - The compiled §7.1 taxonomy (72 entry types, CI-checked against the LLD
    §25 text).
  - The §10.3 actor model (AL-D11 anonymous semantics, iam_system sentinel).
  - `IngestService` + `AuditRepository.Append` (ledger + backstop dedup,
    body-tenant RLS binding).
  - `POST /api/v1/internal/audit-entries` (AL-5, 201/200) and
    `…:batch` (AL-6, 207 per-index, D-4), with per-tenant rate limiting
    (D-5, new `INGEST_BATCH_RATE_LIMIT_RPS` / `_BURST`).
  - Direct-write restricted to §7.1 direct-write types (D-6).
  - Producer contract tests for the realm-provisioner and Catalog clients.
- **Phase 1 — schema & migrations (LLD §4, §19):**
  - `000001_schema` (enums, partitioned `audit_events` + DEFAULT, archive
    state, ledger, plan window, export jobs, redaction tasks).
  - `000002_rls` (fail-closed `tenant_isolation`, FORCE RLS, sampled
    violation log).
  - `000003_roles` (least-privilege matrix; `audit_app` =
    INSERT+SELECT on `audit_events`).
  - `000004_triggers` (append-only backstop).
  - `000005_partition_bootstrap` (`audit_ensure_partitions()` definer
    function + current month ±3).
  - `PartitionService` runs at server startup and in the new `reconcile`
    CronJob (enabled in Helm).
  - Postgres-tier tests: RLS Cases 1–4, grant matrix, triggers,
    constraints, partitions, migration round trip.
- Test tiers mirroring iam-org-membership/test:
  - `test/dbseed`: O&M's seed pool.
  - `test/fixtures`: PG15, audit_app/audit_reconciler roles, and floci
    booted with `scripts/init-floci.sh`.
  - `test/postgres`: role separation and tenant-GUC isolation on a reused
    backend (AL-INV-3), migrations idempotent, no outbox tables
    (AL-INV-10).
  - `test/e2e`: the real router over HTTP on Postgres, plus the real
    binaries run as processes (production boot as audit_app, refusing the
    reconciler role, drain on SIGTERM).
  - `test/integration`: floci topology vs LLD §7.1, raw SNS → platform-events
    wire path, DLQ redrive at maxReceiveCount=5, and the pinned D-3
    malformed-envelope behaviour.
  - `test/unit`: black-box consistency checks across the queue catalog,
    AsyncAPI, init-floci.sh, IAM policy, Helm, `.env-example`, migrations,
    the Dockerfile, and the §17/§10.3 constants.
- Test coverage raised to 96.8% merged (unit + postgres + integration); the
  CI gate is back at 95%, matching iam-org-membership.
- Phase 0 scaffolding (LLD §3, §12, §13): `cmd/server` + `cmd/reconciler`
  composition roots, config loading, two DB pools on separate roles
  (`audit_app` RLS-bound with transaction-local `app.tenant_id` GUC binding;
  `audit_reconciler` BYPASSRLS), migration runner, `/healthz` `/readyz`
  `/metrics`, receive-only AsyncAPI skeleton (11 channels), Dockerfile (both
  binaries), Helm chart (per-root Secrets), IRSA policy, floci dev stack.
- CI gates: `go-arch-lint` (§3.2, deny-by-default vendors for the core),
  `check-grants.sh` (AL-INV-1), `check-forbidden-events-bypass.sh`
  (AL-INV-10), `check-asyncapi-receive-only.sh` (AL-EVT-1),
  `check-metric-naming.sh` (§11).
- `.github/` aligned with iam-org-membership: its `ci.yml`,
  `validate-test.yml`, `validate-quality.yml`, `release.yml` and
  `changelog-check.yml` are the base, with only service-specific deltas
  (two binaries, `go_private_token` build secret, `production-data-migrations`
  deploy environment, per-root Helm Secrets, `iam` namespace). Also imported
  its `check-metric-naming.sh` (business.go / tier-label rules),
  `metrics-registry-lint.sh`, `check-outbox-access.sh` (made strict: no outbox
  at all), `dependabot.yml.disabled`, and its governance-template structure.
  Added `make cover-func`, `make pin-base-images` and `.docker-digests`.
- `internal/adapter/outbound/metrics/business.go` (O&M pattern): Tier-1
  `platform_*` consumer/DLQ/dependency collectors and Tier-2
  `iam_rls_violations_total`, with centrally injected tier ConstLabels.

### Fixed
- **AL-INV-3:** the audit_app pool now forces pgcommon's PgBouncer
  (transaction-local) GUC path. With `PG_BOUNCER_MODE=false` (the dev
  default), pgcommon set `app.tenant_id` session-level in a PrepareConn
  hook, so one tenant's GUC survived COMMIT and leaked to the next checkout
  of that backend. Caught by `test/postgres` TestGUC_*.
- `cmd/reconciler`: `--job` used the global flag set, so a second `run()` in
  one process panicked ("flag redefined"); now a local `FlagSet`.
- `cmd/server`: platform-events / pgmetrics metric init is guarded by
  `sync.Once` (a repeated init panicked on duplicate registration).
