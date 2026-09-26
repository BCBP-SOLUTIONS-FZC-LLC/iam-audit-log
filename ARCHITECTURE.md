# Architecture — iam-audit-log

Ports-and-adapters (LLD §3), enforced by `.go-arch-lint.yml`.

```
cmd/server      ── query+ingest API (Gin) + SQS consumer fleet ── role audit_app (INSERT+SELECT, RLS)
cmd/reconciler  ── archival / partitions / prune CronJob       ── role audit_reconciler (BYPASSRLS, sole DELETE)
        │
        ├── internal/core/domain    stdlib only: entry, actor, taxonomy, tiers, §17 errors
        ├── internal/core/port      interfaces the core needs
        ├── internal/core/service   use cases (Ingest, Query, Export, Archive, Retention, Partition, Redaction)
        ├── internal/adapter/inbound/{http,consumer}
        └── internal/adapter/outbound/{postgres,s3,glue,metrics,catalog}
```

Key rules:

| Rule | Where enforced |
|---|---|
| Core imports no framework/vendor code (§3.2) | `.go-arch-lint.yml` (`depOnAnyVendor: false`) |
| Raw SQL only in `outbound/postgres`; SQS client only in `cmd/server/main.go`; S3 only in `outbound/s3` | `.github/scripts/arch-lint.sh` |
| Each composition root binds only its own DB role (rule 5) | `arch-lint.sh`, `internal/config`, per-root Helm Secrets, startup role check |
| Tenant GUC only transaction-local (AL-INV-3) | `IdentityBridgeMiddleware` → `pgcommon.WithGUCSet`; `validate-quality.yml` grep |
| No publisher/outbox (AL-INV-10) | `check-forbidden-events-bypass.sh`, `check-asyncapi-receive-only.sh` |
| `audit_app` = INSERT+SELECT on `audit_events` (AL-INV-1) | `check-grants.sh` + postgres grant test |

Middleware order (§3.3.1): Timeout → Observability (PanicRecovery → RequestID → Tracing → CorrelationHeaders → Metrics → Logging) → RequireAuth → Context → IdentityBridge → RequireAuditReader | RequireSystemRole.

## Database (Phase 1)

Migrations `000001`–`000005` under `internal/adapter/outbound/postgres/migrations/`
(schema → RLS → roles → triggers → partition bootstrap). Partition DDL for the
runtime roles goes through `audit_ensure_partitions()` (SECURITY DEFINER,
migrator-owned, bounded window); `audit_app` never touches a partition
directly — only the RLS-protected parent.
