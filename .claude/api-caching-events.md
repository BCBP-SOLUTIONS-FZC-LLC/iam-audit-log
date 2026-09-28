# API, caching and events

Swagger: `docs/swagger` (`make swag`). Router: `internal/adapter/inbound/http/router.go`. Identity comes from the gateway headers `x-user-id`, `x-tenant-id`, `x-tenant-roles`; it is never taken from the path or body. Errors use the §17 envelope `{error, message, status, trace_id, request_id, details?}`.

## Routes

| # | Route | Auth | Notes |
|---|---|---|---|
| AL-1 | `GET /api/v1/audit/events` | `tenant_admin` / `tenant_owner` (`RequireAuditReader`), else 403 `insufficient_permissions` | Keyset `(occurred_at DESC, id DESC)` with an opaque cursor; limit ≤ 1000. The plan window is clamped (`window_clamped`, `effective_from`). Months with sealed archive objects are read from S3. If the archived estimate exceeds `ARCHIVE_SYNC_MAX_ROWS` / `_BYTES`, an export is auto-created and the call returns **202** (D-10). |
| AL-2 | `GET /api/v1/audit/events/:id` | same | Hot read first, then an archive fallback limited to objects whose id range holds the id (gap 32). Outside the window → 404 `audit_entry_not_found`. |
| AL-3 | `POST /api/v1/audit/exports` | same | 202 `{export_id, status, status_url}`. The filter is stored clamped. Per-tenant limit → 429 `rate_limited` + `Retry-After`. No dedup (gap 31). |
| AL-4 | `GET /api/v1/audit/exports/:id` | same | When ready, returns a fresh short-lived `download_url` (`EXPORT_DOWNLOAD_URL_TTL`) valid until `expires_at` (7 d, D-11). A lapsed export shows `expired`. |
| AL-5 | `POST /api/v1/internal/audit-entries` | mesh `iam-system` role (`RequireSystemRole`), else 403 `forbidden_peer` | `Idempotency-Key` is required and becomes `source_event_id`. 201 created, 200 replay. 422 for `unknown_entry_type`, `metadata_too_large`, `invalid_actor`. Only direct-write entry types are accepted (D-6). |
| AL-6 | `POST /api/v1/internal/audit-entries:batch` | same + per-tenant limiter (D-5) | Each entry carries its own `idempotency_key`; always **207** with per-index results (D-4); 422 `batch_too_large`. |
| AL-7 | `GET /api/v1/internal/audit/events?tenant_id=` | mesh `iam-system` | Explicit tenant (RLS-bound), no plan clamp. An archived range over the bounds → 422 `range_too_large` (D-14). |

Also `/healthz`, `/readyz` (Postgres plus consumers), `/asyncapi[.yaml]`, and `/metrics` on the metrics port.

## Caching

None (AL-D5). The only in-process state is the CAT-I2 plan map, a stale-if-error snapshot and not a request cache.

## Events (consume-only)

- **Publisher/outbox:** none (AL-INV-10). `api/asyncapi.yaml` has only receive operations.
- **Queues** (`internal/config/config.go` `InboundQueues`), each with a `-dlq`:

  | Queue | Topic |
  |---|---|
  | `auth-audit-q` | `iam.auth.events` |
  | `user-audit-q` | `iam.user.events` |
  | `membership-audit-q` | `iam.membership.events` |
  | `tenant-audit-q` | `iam.tenant.events` |
  | `delegation-audit-q` | `iam.delegation.events` |
  | `serviceaccount-audit-q` | `iam.serviceaccount.events` |
  | `tender-audit-q` | `tender.events` |
  | `billing-audit-q` | `billing.events` |
  | `usage-audit-q` | `usage.events` |
  | `wf-workflow-audit-q` | `wf.workflow.events` |
  | `wf-template-audit-q` | `wf.template.events` |

- **Consumer:** the platform-events `NewSQSConsumer`, configured by `config.LoadSQS`. The Glue codec validates against the producer's registered schema version (AL-D12). Decode failure → redelivery → DLQ.
- **Taxonomy:** `internal/core/domain/taxonomy.go`. An unknown type is persisted as `<domain>.unknown` with `security_3y` (AL-EVT-4). The workflow wire aliases are D-7; actor/target derivation is D-8; oversized payloads get a truncation marker (D-9).
- **Dedup:** handlers are idempotent on `events.Envelope.ID`, the platform-events contract. The key goes into the `processed_events` ledger, backed by the `(source_event_id, occurred_at)` unique constraint.
- **Malformed envelopes:** the library deletes a malformed envelope rather than DLQing it (D-3, gap 42 upstream). It is alerted via `events_consumed_total{status="malformed"}`.
- **Redaction trigger:** `UserDeleted` on `user-audit-q` also creates a redaction task (see [request-flows.md](request-flows.md)). `TenantOffboarded` does not (D-16).
