# Architecture diagrams

Standalone Mermaid source files. [`ARCHITECTURE.md`](../../ARCHITECTURE.md) embeds every one of these `.mmd` files verbatim as a fenced code block, each under a `> Source:` link back to the file here, so a diagram only needs to be correct in one place. Keep both in sync by hand when either changes. The LLD ([`docs/lld/iam-lld-audit-log-service.md`](../lld/iam-lld-audit-log-service.md)) stays authoritative. `D-n` refers to a decision and `gap n` to a finding in [`BUILD_PLAN.md`](../implementation/BUILD_PLAN.md) §C.

**Legend:** a solid arrow is a synchronous call and a dashed arrow is async or background. `audit_app` is the RLS-bound runtime role and `audit_reconciler` is BYPASSRLS; definer functions are shown with `()`.

| File | Diagram | Related section |
|------|---------|------------|
| [`components.mmd`](mermaid/components.mmd) | Components and deployment: server workers, CronJobs, Postgres, S3, SQS, Glue, Catalog, Prometheus | LLD §3, §13 |
| [`layer-model.mmd`](mermaid/layer-model.mmd) | Clean Architecture layer graph | ARCHITECTURE.md § Layer model |
| [`package-dependencies.mmd`](mermaid/package-dependencies.mmd) | Go import graph (mirrors `.go-arch-lint.yml`) | `.go-arch-lint.yml` |
| [`request-flow.mmd`](mermaid/request-flow.mmd) | AL-1 query lifecycle: middlewares → plan window → hot RDS + sealed S3 → D-10 202 | LLD §5.4, §8.4 |
| [`write-flow.mmd`](mermaid/write-flow.mmd) | The single `Append` path: ledger → D-18 check → insert/backstop → redaction task (no outbox) | LLD §8.1, §8.2, §8.7 |
| [`archival-flow.mmd`](mermaid/archival-flow.mmd) | reconcile: partitions → re-open → archive → verify → `audit_drop_partition()` gate | LLD §8.5, §15.4 |
| [`archive-state-machine.mmd`](mermaid/archive-state-machine.mmd) | `audit_event_archive_state` transitions incl. re-archive and re-open (D-19/D-20) | LLD §4.2, §8.5 |
| [`redaction-flow.mmd`](mermaid/redaction-flow.mmd) | GDPR redaction: immediate / retry / D-18 ingest check / sweep / invalidation / missed | LLD §8.7, §15.5 |
| [`cache-strategy.mmd`](mermaid/cache-strategy.mmd) | No request-path cache (AL-D5); the CAT-I2 plan-window map (stale-if-error); export presign | LLD §6, AL-D15 |
| [`data-model.mmd`](mermaid/data-model.mmd) | ERD of every table (migrations 000001–000010) | LLD §4.2 |
| [`event-consumption-flow.mmd`](mermaid/event-consumption-flow.mmd) | SNS → 11 SQS queues → platform-events consumer → codec → handler → ledger; DLQ; no publisher | LLD §7 |
| [`observability-stack.mmd`](mermaid/observability-stack.mmd) | gincommon chain, telemetry seam, three metric tiers, D-21 gauges, rules, CI conformance | LLD §11 |
| [`rls-guc-flow.mmd`](mermaid/rls-guc-flow.mmd) | `app.tenant_id` transaction-local GUC injection, fail-closed RLS, definer functions | LLD §4.3 |
| [`docs-assets.mmd`](mermaid/docs-assets.mmd) | Documentation sources and renderers | ARCHITECTURE.md § Documentation assets |

To render locally, open any `.mmd` file in a Mermaid-aware IDE (VS Code + Mermaid Preview, IntelliJ + Mermaid plugin) or paste it into [mermaid.live](https://mermaid.live).
