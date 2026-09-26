# Contributing — iam-audit-log

The LLD (`docs/lld/iam-lld-audit-log-service.md`) is the single source of
truth. Cite the section / ID (AL-INV-*, AL-D*, AL-1..AL-7) you implement in the
PR. Do not diverge from it in code; raise a spec gap instead
(`docs/implementation/BUILD_PLAN.md` §C tracks them).

## Setup

```sh
make setup        # .env + git hooks
make docker-up    # Postgres 15 (:5548) + floci (:4573)
make run          # cmd/server
make run-reconciler JOB=<name>
```

## Test layout (mirrors iam-org-membership/test)

| Tier | Where | Tag | Needs |
|---|---|---|---|
| Unit — white-box | next to the code | — | nothing |
| Unit — black-box contracts | `test/unit` (package `unit_test`) | — | nothing |
| Postgres (roles / GUC / RLS / partitions) | `test/postgres`, `cmd/*/*_integration_test.go`, `internal/adapter/outbound/postgres/*_integration_test.go` | `integration` | Docker |
| Integration (SNS/SQS/S3/Glue on floci) | `test/integration` (one floci per package, provisioned by `scripts/init-floci.sh`) | `integration` | Docker |
| e2e (real router over HTTP + real binaries) | `test/e2e` | `e2e` | Docker, Go toolchain |

Shared harness: `test/dbseed` (superuser seed pool), `test/fixtures`
(`StartPostgres`, `CreateRoles`, `StartFloci`/`NewFloci`).

Every invariant (AL-INV-1..12) gets a test that names it (e.g.
`TestRLS_Case2_CrossTenantInsertRejected_ALINV3`).

## Non-negotiables

- No `UPDATE`/`DELETE` path for `audit_app` (AL-INV-1).
- No SNS publisher / outbox, ever (AL-INV-10) — CI fails the build.
- `app.tenant_id` only via `pgcommon.WithGUCSet` (transaction-local); never `SET` at session level.
- `cmd/server` never uses the reconciler DSN; `cmd/reconciler` never uses the app DSN.
- No audit payload / `metadata` in logs at info level (§11).
