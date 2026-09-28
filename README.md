# iam-audit-log

The Audit Log Service is the **compliance system of record** for the Tender Management SaaS platform. It keeps one append-only, tenant-scoped store that answers *"who did what, to what, when, and from where"* for every IAM and domain service. It is a **pure sink**: it consumes every audit-bearing SNS topic through 11 SQS queues and accepts a small set of non-bus "direct audit writes" over the mesh. It publishes nothing and has no outbox (AL-INV-10). Tenant admins read their trail through a plan-window-clamped query and export API. Compliance callers read it through a mesh-only provenance endpoint.

**Repository:** `github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log`
**Module:** Go 1.26.6 · private module · deployed as two binaries from one image (`cmd/server`, `cmd/reconciler`)
**Design:** Refines the IAM HLD (§5.7 charter, §6.6 plan-gated query window, §9.1/§9.4 topology and event catalogue, §13 retention/GDPR). The LLD is `docs/lld/iam-lld-audit-log-service.md`; it is at **rev 0.26** (as-built through Phase 8 and the platform-library / observability-standard alignment). Where the LLD and HLD disagree, the HLD is authoritative. Build decisions (D-1..D-21) and the spec-gap register (gaps 1–46) are in [`docs/implementation/BUILD_PLAN.md`](docs/implementation/BUILD_PLAN.md). Phases 0–8 are implemented.

---

## Mental model

| This service owns | It does NOT own |
|---|---|
| `audit_events`: the canonical audit row, partitioned monthly on `occurred_at`, with `FORCE ROW LEVEL SECURITY` and append-only storage. `audit_app` has INSERT+SELECT only, and a trigger blocks UPDATE/DELETE (AL-INV-1). | The business events themselves. Each producer owns its topic, schema and Glue registry. |
| The unified `entry_type` taxonomy and its retention tiers (`compliance_7y`, `security_3y`, `access_90d`). The tier is derived from `entry_type` and is never caller-supplied (AL-INV-6/11). | Any SNS topic, outbox or published event. This service produces nothing (AL-INV-10). |
| `processed_events`: the dedup ledger keyed on the envelope `id` or the direct-write `Idempotency-Key` (AL-INV-4) | Plan definitions and `audit_query_window_days`, which belong to Catalog / Admin Config (read through CAT-I2) |
| `tenant_plan_window`: each tenant's current plan, projected from Billing events and used for the query clamp (AL-INV-8) | Tenant, membership and role state (Org & Membership) |
| `audit_event_archive_state` / `audit_archive_objects`: the monthly archive lifecycle and the sealed S3 manifest (D-12/D-20) | User identity or display data. This service stores the actor UUID plus an optional display label, never a profile. |
| `audit_export_jobs`: AL-3/AL-4 async exports, SSE-KMS encrypted, served through a fresh presigned URL on every AL-4 poll (D-11) | Deleting a user. `UserDeleted` triggers *redaction* here; the erasure itself happens upstream. |
| `audit_redaction_tasks` / `redacted_subjects`: GDPR redaction of non-`compliance_7y` rows (D-15..D-18, AL-INV-7/12) | Authentication, token issuance, JWT parsing (Keycloak and the gateway) |
| `rls_violation_log`, `ops_export_watermark`: security and ops signals behind the D-21 gauges | Consumers of the trail. Nothing depends on this service synchronously on a hot path. |

The trail has two ingest paths that produce **the same row shape** (AL-INV-2):

- **Bus** (`ingest_mode='bus'`): the 11-queue consumer fleet.
- **Direct-write** (`ingest_mode='direct_write'`): AL-5/AL-6, for the 16 taxonomy types that have no bus event (AL-D1, D-6).

Neither path can mutate or delete a row. The only ways data leaves the hot store are:

- the reconciler archiving a month to S3 (Object Lock COMPLIANCE) and dropping the partition through the in-database AL-INV-9 gate
- GDPR redaction, which scrubs PII fields in place and never deletes the row

---

## Why this service exists

Every IAM and domain service performs actions a compliance reviewer must later reconstruct: role grants, tender approvals, IdP changes, cross-tenant operator sessions, service-account key rotations. Without a single sink, each service would keep its own audit table with its own retention, its own tenancy rules and its own export format. Centralizing it here means:

- **One retention model.** The tier comes from `entry_type` via one taxonomy (`internal/core/domain/taxonomy.go`). A tender approval is kept 7 years wherever it came from, and no caller can shorten that.
- **One tenancy boundary.** RLS on `app.tenant_id` is bound transaction-locally on every checkout (AL-INV-3), so a tenant admin can never read another tenant's trail, even through a bug in the query layer.
- **One idempotency contract.** At-least-once bus delivery and producer retries are both absorbed by one ledger (`processed_events`), so an audit fact is recorded exactly once (AL-INV-4).
- **One place for the plan clamp.** How far back a tenant may query (Starter 1 year, and so on) is a *read* policy applied at query time (AL-INV-8). It never shortens *storage*. Older rows stay retrievable through export (AL-3) and the internal read (AL-7).
- **One archival and erasure story.** Monthly partitions move to S3 with Object Lock, are verified, and only then are dropped. GDPR redaction reaches hot rows immediately and is backed by retry and sweep jobs.

This is enforced structurally. `internal/core/service` depends only on `internal/core/domain` and `internal/core/port`, never on a vendor type. `.go-arch-lint.yml`, together with the grep gates in `.github/scripts/`, encodes the dependency and confinement rules CI checks on every PR.

---

## API overview

**7 business endpoints** across two route prefixes, plus the infra and docs routes. The source of truth is `internal/adapter/inbound/http/router.go`. The generated REST contract is `docs/swagger/swagger.yaml` (`make swag`); it is not hand-authored. The consumed-event contract is `api/asyncapi.yaml`, which has receive operations only.

**Middleware chain (router.go):**

1. `MaxBytesReader` (8 MiB)
2. `gincommon.TimeoutMiddleware(30s)`
3. `gincommon.ObservabilityMiddlewares`: `PanicRecovery → RequestID → Tracing → CorrelationHeaders → Metrics → Logging`
4. On both prefixes: `gincommon.ProtectedMiddlewares` (`RequireAuth → Context`), then `IdentityBridgeMiddleware`, then `RequireJSONContentType`
5. Then a per-prefix role gate (listed with each prefix below)

`IdentityBridgeMiddleware` parses the identity headers into `pkg/requestctx` and binds `app.tenant_id` transaction-locally. The service trusts the gateway- or mesh-injected `x-user-id`, `x-tenant-id` and `x-tenant-roles` headers. There is no JWT parsing.

### Public routes (4): `/api/v1/audit/*`, `tenant_admin` or `tenant_owner` only

`RequireAuditReader` gates this prefix. A plain member gets `403 insufficient_permissions`.

| # | Method & Path | Purpose |
|---|---|---|
| AL-1 | `GET /audit/events` | Query the tenant's trail: filters, keyset pagination (`limit` default 100, max 1000; opaque `cursor`), clamped to the plan window. The result merges hot rows with sealed archive objects. An archived range over `ARCHIVE_SYNC_MAX_ROWS`/`_BYTES` becomes a **`202` export** instead of a page (D-10). |
| AL-2 | `GET /audit/events/:id` | A single entry by UUID (`404 audit_entry_not_found`) |
| AL-3 | `POST /audit/exports` | Start an async export of a filter (always `202`, with `export_id` and `status_url`). Rate-limited per tenant (`429 rate_limited`). |
| AL-4 | `GET /audit/exports/:id` | Export status (`pending`, `running`, `ready`, `failed`, `expired`). When ready, the response carries a **fresh** presigned `download_url` (TTL `EXPORT_DOWNLOAD_URL_TTL`) on every poll (D-11). |

AL-1 query parameters are `from`, `to`, `entry_type` (repeatable), `actor_id`, `actor_type`, `target_type`, `target_id` (requires `target_type`), `source_service`, `retention_tier`, `limit` and `cursor`. A response carries `events`, `next_cursor`, `window_clamped` and `effective_from`. If a query reaches back past the plan window, the response is `200` with an empty or truncated page and `window_clamped: true`; it is not an error.

### Internal routes (3): `/api/v1/internal/*`, mesh mTLS, NetworkPolicy, `iam-system` role

`RequireSystemRole` gates this prefix. An unrecognized peer gets `403 forbidden_peer`.

| # | Method & Path | Caller | Purpose |
|---|---|---|---|
| AL-5 | `POST /internal/audit-entries` | Direct-write producers (via `platform-audit`) | Record one direct-write entry. **`Idempotency-Key` header required.** Returns `201` for a new entry or `200` for an idempotent replay. |
| AL-6 | `POST /internal/audit-entries:batch` | Direct-write producers (for example Group Mapping reconciles) | Up to `MAX_INGEST_BATCH` (500) entries. The `Idempotency-Key` header is required, and so is each entry's `idempotency_key` (D-4). The response is **always `207`** with a per-index `{index, status, id \| code, message}`. Rate-limited per tenant (D-5). |
| AL-7 | `GET /internal/audit/events?tenant_id=` | Compliance and verification callers | Provenance read for an explicit `tenant_id`. RLS-scoped to that tenant, **no plan clamp**. Returns `422 range_too_large` instead of `202` when the archived range exceeds the D-10 bounds (D-14). |

AL-6's path contains a literal colon. Gin routes it with an escaped segment (`/audit-entries\:batch`), so callers send `…/audit-entries:batch` as written.

### Infra and docs routes: no auth

| Method & Path | Purpose |
|---|---|
| `GET /healthz` | Liveness: `200 {"status":"ok"}`. It never inspects a dependency. |
| `GET /readyz` | Readiness: the `database` and `consumers` checks run concurrently. Returns `200` or `503` with per-check `ok`/`down`. |
| `GET /asyncapi` / `GET /asyncapi.yaml` | The embedded AsyncAPI spec as JSON or YAML. Mounted everywhere outside production. In production it is mounted only with `DOCS_ENABLED=true`, and bearer-gated if `DOCS_AUTH_TOKEN` is set. |
| `GET :9090/metrics` | Prometheus, on the separate `METRICS_PORT` listener and not on the API port |

---

## Input validation

A domain-rule failure is a `*domain.Error` (`internal/core/domain/errors.go`) carrying one of the frozen §17 codes. `HandleError` (`internal/adapter/inbound/http/errors.go`) maps the code to its fixed HTTP status. A connectivity-class Postgres error that escapes untranslated becomes `503 dependency_unavailable`. Anything else becomes `500 internal_error` and is logged.

The error body is a superset of `gincommon.ErrorResponse`: `error` and `code` both carry the §17 code, and structured details sit under a nested `details` object.

```json
{
  "error": "metadata_too_large",
  "code": "metadata_too_large",
  "message": "metadata exceeds the size cap",
  "status": 422,
  "trace_id": "4bf92f3577b34da6a3ce929d0e0e4736",
  "request_id": "0192…",
  "details": { "limit_bytes": 8192, "size_bytes": 9120 }
}
```

| Code | Status |
|---|---|
| `invalid_request` | 400 |
| `missing_identity_headers` | 401 |
| `insufficient_permissions` / `forbidden_peer` | 403 |
| `audit_entry_not_found` / `export_not_found` | 404 |
| `unknown_entry_type` / `invalid_actor` / `metadata_too_large` / `batch_too_large` / `range_too_large` | 422 |
| `rate_limited` | 429 |
| `dependency_unavailable` | 503 |

### Notable validation rules

| Rule | Enforcement |
|---|---|
| Identity comes only from headers. The body is never trusted for *authorization*. | `x-user-id` must be a UUID or the `iam-system` principal (`…0000000000a1` or the literal `iam-system`), and `x-tenant-id` must be a UUID, else `401`. AL-5 records the body's `tenant_id` and AL-7 reads the query's `tenant_id`: both routes are mesh-only and rebind the GUC to that tenant (`BindTenantGUC`). |
| Direct-write accepts only the 16 direct-write taxonomy types | Any other `entry_type` returns `422 unknown_entry_type` (D-6). Bus types cannot be forged through AL-5/AL-6. |
| The retention tier is never caller-supplied | A `retention_tier` in the body is ignored. The tier is derived from `entry_type` (AL-INV-6, AL-INV-11). |
| The actor model is strict | `anonymous` must carry no `id` (AL-D11). Every other type needs a UUID `id`. `iam_system` must use the reserved `00000000-0000-0000-0000-0000000000a1`. A violation returns `422 invalid_actor`. |
| Metadata is bounded | It must be a JSON object and is compacted, then capped at `MAX_METADATA_BYTES` (8192), else `422 metadata_too_large` with `details`. |
| `Idempotency-Key` is required and never minted | Required on AL-5 and AL-6 (1–256 chars), else `400`. The caller's stable key is what makes its retry-until-acked delivery safe (LLD §18.2). |
| POST bodies must be JSON | A non-empty body whose `Content-Type` is not `application/json` returns `400 invalid_request` (`RequireJSONContentType`). |
| Query filters are shape-checked | `from ≤ to`; `actor_id` is a UUID; `actor_type` and `retention_tier` come from their enums; `target_id` requires `target_type`; `1 ≤ limit ≤ 1000`. Anything else returns `400`. |
| Unknown bus types are kept, not dropped | An unrecognized event type is persisted as `<domain>.unknown` at `security_3y` (AL-EVT-4). |

---

## Architecture

Ports and adapters: dependencies point inward, and outer layers are never imported by inner ones. The package map, both composition roots, the enforced rules and the request flows are in **[`ARCHITECTURE.md`](ARCHITECTURE.md)**. Standalone Mermaid diagrams (components, ingest, query routing, archival, redaction) live in **[`docs/architecture/`](docs/architecture/README.md)**.

```
iam-audit-log/
├── cmd/
│   ├── server/                        # Composition root: pgcommon pool (audit_app) + migrations, 11-queue consumer fleet,
│   │                                  #   CAT-I2 plan poller, export worker, OpsMonitor gauges (D-21), API :8080 + /metrics :9090
│   └── reconciler/                    # One binary, --job=<name>; jobs/ holds reconcile, redaction-retry, redaction-sweep,
│                                      #   processed-events-prune
├── internal/
│   ├── config/                        # Env parsing + fail-fast validation for both roots (LLD §12)
│   ├── eventschema/                   # Embedded consumed-event schemas
│   ├── core/
│   │   ├── domain/                    # AuditEntry, actor model, entry_type taxonomy + tiers, §17 errors: stdlib only
│   │   ├── port/                      # Repositories, archive store, catalog client, logger/metrics/tracing ports
│   │   └── service/                   # Ingest (direct-write), bus ingest + redaction, query, export, archive,
│   │                                  #   partition, plan poller, ops monitor
│   └── adapter/
│       ├── inbound/
│       │   ├── http/                  # router.go, AL-1..AL-7 handlers, DTOs, middleware, rate limiters, AsyncAPI
│       │   └── consumer/              # SQS consumer fleet: one platform-events consumer per *-audit-q
│       └── outbound/
│           ├── postgres/              # pgcommon pools, repositories, migrations 000001..000010
│           ├── s3/                    # Archive objects + exports (SSE-KMS, Object Lock, presign)
│           ├── glue/                  # Decode-only Glue Schema Registry codec (AL-D12)
│           ├── catalog/               # CAT-I2 client (GET /api/v1/internal/plans)
│           ├── metrics/               # Three-tier Prometheus collectors + registry.go ledger
│           └── telemetry/             # The only home for OTel / promhttp wiring (gap 43)
├── pkg/requestctx/                    # Typed RequestContext{UserID, TenantID, Roles, ClientIP, UserAgent}
├── api/asyncapi.yaml                  # 11 consumed channels, receive-only
├── docs/
│   ├── lld/                           # The spec: §17 error taxonomy, §16 open questions, §22 decisions
│   ├── implementation/                # BUILD_PLAN.md (phases, D-1..D-21, gaps), RELEASE_CHECKLIST.md
│   ├── architecture/                  # Mermaid diagrams
│   ├── swagger/                       # Generated REST contract (make swag)
│   └── runbook.md                     # RB-1..RB-10
├── deploy/                            # helm/ (Deployment + 4 CronJobs), monitoring/ (rules, registry, dashboard), iam/policy.json
├── scripts/                           # init-floci.sh, merge_coverage.py, gen-prometheusrule.py
├── .githooks/pre-commit               # tidy + fmt-check + vet + lint; installed by make setup
└── test/                              # unit (black-box contracts), postgres (RLS/grants/partitions), integration (floci),
                                       #   e2e (router + real binaries), fixtures, dbseed
```

### Dependency rules (enforced by `go-arch-lint` in CI)

| Component | May depend on |
|---|---|
| `domain` | Nothing internal (stdlib only) |
| `port` | `domain` |
| `service` | `domain`, `port` |
| `http` (inbound) | `domain`, `port`, `service`, `requestctx`, `apispec` |
| `consumer` (inbound) | `domain`, `port`, `service`, `eventschema` |
| `postgres` / `s3` / `metrics` / `catalog` (outbound) | `domain`, `port` |
| `glue` (outbound) | `domain`, `port`, `eventschema` |
| `telemetry` (outbound) | `port` |
| `jobs` (`cmd/reconciler/jobs`) | `domain`, `port`, `service` |
| `server` (`cmd/server`) | Everything above plus `config` |
| `reconciler` (`cmd/reconciler`) | `domain`, `port`, `service`, `config`, `jobs`, `postgres`, `s3`, `metrics`, `telemetry` |

`.github/scripts/arch-lint.sh` adds grep gates on top: raw SQL only in `outbound/postgres`, SQS/S3/Glue SDK confinement, and per-root DB-role separation. The server never sees the reconciler DSN, and the reconciler never sees the app DSN.

**AL-INV-3:** `app.tenant_id` is bound with `set_config(…, true)` (transaction-local) inside every transaction and is never session-level. The app pool **forces** `PGBouncerMode=true` whatever `PG_BOUNCER_MODE` says, because pgcommon only takes the transaction-local path in that mode (`internal/adapter/outbound/postgres/db.go`). CI also rejects any non-`LOCAL` `SET app.tenant_id`.

### Storage and messaging

| Concern | Technology | Notes |
|---|---|---|
| **Primary store** | PostgreSQL 15, database `audit` (PgBouncer transaction pooling in production) | `audit_events` is partitioned monthly on `occurred_at`, with a DEFAULT partition as a safety net. `FORCE ROW LEVEL SECURITY` and GUC `app.tenant_id`. Three roles: `audit_app` (server, INSERT+SELECT on `audit_events`, RLS-bound), `audit_reconciler` (BYPASSRLS, jobs only) and `audit_migrator` (DDL). |
| **Cache** | None | No cache means no invalidation cost. Every read hits Postgres or S3, which is the right posture for a compliance store (LLD §6, §21). |
| **Events (outbound)** | **None** | No SNS publisher, no outbox (AL-INV-10). The server warns and ignores `SNS_TOPIC_ARN`/`OUTBOX_DATABASE_URL` if they are set. |
| **Events (inbound)** | AWS SQS, 11 queues | `<topic>-audit-q`, each with a `-dlq` and `maxReceiveCount=5`. Each is subscribed catch-all (no filter policy, AL-EVT-2) with `RawMessageDelivery=true`. Consumed through `platform-events`. Dedup is on `Envelope.ID` in the same transaction as the insert. |
| **Schema registry** | AWS Glue, 6 IAM registries (read-only) | The codec is invoked only when the envelope's `dataschema` is set. A plain-JSON envelope skips it (AL-D12). This service registers no schemas. |
| **Archive / exports** | AWS S3 `iam-audit-archive`, SSE-KMS `alias/iam-audit-archive`, Object Lock `COMPLIANCE` | Per-tenant, per-tier parts (`ARCHIVE_PART_MAX_ROWS`) go into a sealed manifest (D-12/D-20). Exports are written to the same bucket and read back through presigned URLs. |

### Shared library dependencies

| Library | Version | Purpose | Confinement rule (CI) |
|---|---|---|---|
| `platform-gincommon` | v1.3.0 | HTTP middleware, Zap logger, OTel tracing, the Prometheus registry | **All logs, metrics and traces go through gincommon only**. OTel and promhttp are wired only in `outbound/telemetry` (gap 43, `check-observability-confinement.sh`). |
| `platform-pgcommon` | v1.3.0 | pgx/v5 pools, `ConfigFromEnv`, GUC injection, transactions, error classification, migrations | **Every DB connection, config value and operation goes through pgcommon only** (gap 44, `arch-lint.sh`) |
| `platform-events` | v1.4.0 | SQS consumer, envelope, `config.LoadSQS`, dedup | **Events, SQS config and dedup go through platform-events only**, with no publisher or outbox APIs (gap 45, `check-forbidden-events-bypass.sh`, `check-outbox-access.sh`) |

---

## Integrating with other services

### 1. Prerequisites

Every caller of `/api/v1/internal/*` must run on the internal service mesh (mTLS plus NetworkPolicy) and present the `iam-system` role in `x-tenant-roles`. The public `/api/v1/audit/*` routes expect the gateway to forward the validated `x-user-id`, `x-tenant-id` and `x-tenant-roles` headers. This service does not parse JWTs. The mesh address is **`http://iam-audit-log.iam.svc.cluster.local:8080`** (LLD §18.2, AL-Q17).

### 2. Bus producers: publish to your own topic, and nothing else is needed

Every producer below publishes to its own topic as usual. This service owns the catch-all audit subscription on each one (LLD §18.1):

| Producer | Topic | Audit queue |
|---|---|---|
| Event Consumer | `iam.auth.events` | `auth-audit-q` |
| User Profile | `iam.user.events` | `user-audit-q` |
| Org & Membership | `iam.membership.events` | `membership-audit-q` |
| Realm Provisioner + Org & Membership | `iam.tenant.events` | `tenant-audit-q` |
| Delegation | `iam.delegation.events` | `delegation-audit-q` |
| Token Service | `iam.serviceaccount.events` | `serviceaccount-audit-q` |
| Tender Service | `tender.events` | `tender-audit-q` |
| Billing | `billing.events` | `billing-audit-q` (also drives `tenant_plan_window`) |
| Usage & Metering | `usage.events` | `usage-audit-q` |
| Workflow Engine | `wf.workflow.events`, `wf.template.events` | `wf-workflow-audit-q`, `wf-template-audit-q` |

**Contract for producers:**

- The envelope `id` must be stable across redeliveries. It is the dedup key.
- `tenant_id` must be a UUID. An envelope with no `id` or a non-UUID `tenant_id` cannot be stored. It is retried and then redriven to the DLQ (RB-1).
- A Glue-framed payload must set `dataschema`.
- A new event type needs no change here to be *kept*: it lands as `<domain>.unknown`. Getting a proper `entry_type` and tier needs a taxonomy entry in `internal/core/domain/taxonomy.go` and `api/asyncapi.yaml`.

### 3. Direct-write producers: AL-5 / AL-6 via `platform-audit`

This path is for audit facts that have no bus event (AL-D1). The **caller invariant (AUDIT-CALLER-1)** is that audit capture must never fail the producer's primary write. The producer commits first, then records the entry fire-and-forget with a bounded retry buffer. Use `uuidv5(<service-namespace>, <natural key of the change>)` as the idempotency key.

| Producer | `entry_type`s |
|---|---|
| Catalog / Admin Config | `config.department.created`, `config.department.updated`, `config.plan.updated`. These are platform-wide, so they are recorded under the reserved platform tenant `00000000-0000-0000-0000-0000000000b1` (AL-D14), which no real tenant can see. |
| Group Mapping / JIT Config | `config.group_mapping.departments.changed`, `…department_roles.changed`, `…tenant_roles.changed`, via `:batch` |
| Tender ACL | `config.tender_acl.granted`, `.revoked`, `.cascade` |
| Org & Membership | `config.tenant_setting.changed`, `invitation.created`, `.revoked`, `.expired` |
| Realm Provisioner | `config.idp.changed` |
| Signup / onboarding | `tenant.owner_signed_up` |
| Operator tooling | `security.cross_tenant_access` on every `admin_readonly` session |

### 4. Catalog / Admin Config: CAT-I2 plan windows (this service calls out)

`cmd/server` polls `GET {CATALOG_BASE_URL}/api/v1/internal/plans` every `CATALOG_PLANS_POLL_INTERVAL` (600 s), with a timeout of `CATALOG_PLANS_POLL_TIMEOUT` (3 s). It presents the `iam-system` identity. The result is a live `plan_code → audit_query_window_days` map (AL-D15, D-13).

The poller is **stale-if-error**. A failed or malformed poll keeps the last good map. A plan the map has never seen falls back to `AUDIT_DEFAULT_QUERY_WINDOW_DAYS` (365). Staleness is exported as `iam_audit_log_catalog_plans_stale_seconds`.

### 5. Compliance and verification callers: AL-7

`GET /api/v1/internal/audit/events?tenant_id=<uuid>` takes the same filters and pagination as AL-1 but applies **no plan clamp**. It is the path by which compliance or legal reaches records older than the tenant's in-product window. It never defers to an export. An archived range over the D-10 bounds is `422 range_too_large`, so narrow `from`/`to` and page through.

### 6. Tenant admins: the product's "View audit log"

The product UI calls AL-1..AL-4 through the gateway with the admin's own identity headers. Queries return at most `limit` rows per page. A large archived range comes back as a `202` pointing at an export. Poll AL-4 for a fresh `download_url` each time rather than caching one.

### 7. Subscribing to events

**There is nothing to subscribe to.** This service publishes no domain events of any kind (AL-INV-10). CI blocks publisher and outbox code (`check-forbidden-events-bypass.sh`, `check-outbox-access.sh`) and any `send` operation in the AsyncAPI spec (`check-asyncapi-receive-only.sh`). If you need to react to an audit-worthy action, subscribe to the *producer's* topic, not to this service.

### 8. Handling errors

Every non-2xx response uses the envelope shown under [Input validation](#input-validation). AL-6 never fails the whole batch for one bad entry: each index carries its own status and code inside the `207`. The complete taxonomy is in `docs/lld/iam-lld-audit-log-service.md` §17.

### 9. Rate limits

Two in-process, per-tenant token buckets (`golang.org/x/time/rate`), held per replica:

| Route | Key | Limit | On exceed |
|---|---|---|---|
| AL-6 `:batch` | caller's `x-tenant-id` | `INGEST_BATCH_RATE_LIMIT_RPS` 10/s, burst `INGEST_BATCH_RATE_LIMIT_BURST` 20 (D-5) | `429 rate_limited`, `Retry-After: 1` |
| AL-3 export, and an AL-1 read deferred to an export | tenant | `EXPORT_RATE_LIMIT_PER_MINUTE` 10/min, burst `EXPORT_RATE_LIMIT_BURST` 5 (§10.5) | `429 rate_limited`, `Retry-After: 60` |

Everything else is throttled, if at all, by the gateway. The Helm `securityPolicy.rateLimit` option exists but is off by default.

### 10. Background reconcilers you may observe

4 CronJobs, each running `cmd/reconciler --job=<name>` against the same image (`deploy/helm/templates/cronjobs.yaml`), all `concurrencyPolicy: Forbid`:

| CronJob (`--job=`) | Schedule | Purpose |
|---|---|---|
| `reconcile` | `0 2 * * *` | Pre-create monthly partitions (`AUDIT_PRECREATE_MONTHS` ahead, `AUDIT_WRITABLE_TRAILING_MONTHS` behind). Re-open dropped months that received late rows (D-19). For each month past `AUDIT_HOT_WINDOW_DAYS`: archive each tier to S3, verify, and drop the partition only through the AL-INV-9 gate, skipping partitions with a pending redaction (AL-INV-12). An undroppable partition makes the job exit non-zero. |
| `redaction-retry` | `*/15 * * * *` | Re-apply redaction tasks pending longer than `REDACTION_RETRY_MIN_AGE` (RB-7) |
| `redaction-sweep` | `30 3 * * *` | Daily re-check of subjects erased within `REDACTION_SWEEP_WINDOW` (90 d, D-18) |
| `processed-events-prune` | `0 3 * * *` | Prune ledger rows older than `PROCESSED_EVENTS_TTL_DAYS` (8, deliberately more than the 7-day SQS lifetime) |

The alerting gauges do not come from these jobs. They are archive stall and lag, DEFAULT-partition rows, stuck redactions, queue and DLQ depth, and `iam_rls_violations_total`. **`OpsMonitor`**, a ticker inside `cmd/server`, refreshes them every `OPS_STATS_INTERVAL` (60 s) from `audit_ops_stats()`, `audit_rls_violation_counts()` and SQS attributes (D-21). The server is always scraped, even when no job has run.

---

## Local development

### Prerequisites

- Go 1.26.6+
- Docker (Postgres 15 plus floci, via `make docker-up`)
- `GOPRIVATE=github.com/BCBP-SOLUTIONS-FZC-LLC/*` (and `GONOSUMDB`) with access to the private `platform-*` modules
- The `aws` CLI is optional. The examples below run it inside the floci container instead.

### Setup

```bash
git clone https://github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log
cd iam-audit-log
make setup       # copies .env-example → .env (if missing) + installs .githooks/pre-commit
make tidy        # go mod tidy
make docker-up   # postgres (:5548) + floci (:4573) — not the app container
make run         # cmd/server: API :8080, /metrics :9090; self-migrates at startup
make run-reconciler JOB=reconcile   # or redaction-retry | redaction-sweep | processed-events-prune
```

In dev, `.env-example` points all three DSNs at the local `postgres` superuser. The app-role check logs a warning instead of failing (`appRoleOK`).

### Common commands

| Command | Description |
|---|---|
| `make setup` | Copy `.env-example` → `.env` and install the pre-commit hook |
| `make tidy` / `make fmt` / `make fmt-check` / `make vet` | Go basics. `fmt-check` mirrors CI and does not modify files. |
| `make lint` | `golangci-lint` via `go tool`: the default build plus the `integration,e2e` tags |
| `make arch-lint` | `.github/scripts/arch-lint.sh`: go-arch-lint plus the confinement grep gates |
| `make invariant-lint` | `check-grants.sh`, `check-forbidden-events-bypass.sh`, `check-asyncapi-receive-only.sh`, `check-metric-naming.sh`, `check-observability-confinement.sh` |
| `make mod-verify` / `make vuln-check` | `go mod verify` / `govulncheck ./internal/... ./pkg/...` |
| `make test` | Unit, then postgres, then integration (Docker required; e2e is separate) |
| `make test-ci` | The same three in parallel, with `-race` and merged `coverage.out` (used in CI) |
| `make test-unit` | Unit and black-box contract tests, no Docker |
| `make test-postgres` | Postgres, RLS, grants, partitions and repositories (testcontainers). Parallelism is capped at `TEST_POSTGRES_PARALLEL` (default 4). |
| `make test-integration` | SQS, SNS, S3 and Glue on floci, with the real consumer fleet and reconciler (`TEST_INTEGRATION_PARALLEL`, default 2) |
| `make test-e2e` | The real router over HTTP plus the real binaries as processes |
| `make race` | All three suites with `-race` and no coverage merge |
| `make run` / `make run-reconciler JOB=<name>` | Run a composition root locally (sources `.env`) |
| `make build` | Compile `bin/iam-audit-log-server` and `bin/iam-audit-log-reconciler` |
| `make cover` / `make cover-func` | Coverage HTML report / per-function summary |
| `make ci` | `tidy` + `fmt-check` + `vet` + `lint` + `arch-lint` + `invariant-lint` + `test-ci` + `build` |
| `make docker-up` / `make docker-down` | Start `postgres` and `floci` / stop the stack |
| `make swag` / `make swag-check` | Regenerate / verify `docs/swagger` |
| `make migrate-create NAME=<x>` | New `NNNNNN_<x>.up.sql`/`.down.sql` pair |
| `make pin-base-images` | Re-pin the Dockerfile base-image digests and write `.docker-digests` |
| `make clean` | Remove `bin/`, `.coverage/` and coverage files |

There is no `make test-smoke`. The image size and startup gate (`.github/scripts/smoke-tests.sh`, 200 MB limit) runs only in `ci.yml`. `make test-ci` plus `make test-e2e` takes more than 10 minutes locally, so run it in the background.

### Running a single test

```bash
go test ./test/unit/... -run TestMetricRegistry_Conformance -v
go test ./test/postgres/... -tags=integration -run TestGUC_TransactionLocalNoLeakAcrossPooledBackend_ALINV3 -v
go test ./test/integration/... -tags=integration -run TestBus_PublishedEventPersistedOnceDespiteRedelivery_ALINV4_ALINV5 -v
go test ./test/e2e/... -tags=e2e -run TestAuditRoutesRoleMatrix -v
```

### Calling the API locally

```bash
# After make docker-up and make run:
TENANT_ID=$(python3 -c 'import uuid; print(uuid.uuid4())')
ADMIN_ID=$(python3 -c 'import uuid; print(uuid.uuid4())')

# AL-5: record one direct-write entry (internal route — normally the platform-audit client)
curl -i -X POST http://localhost:8080/api/v1/internal/audit-entries \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: $(python3 -c 'import uuid; print(uuid.uuid4())')" \
  -H "x-user-id: 00000000-0000-0000-0000-0000000000a1" \
  -H "x-tenant-id: $TENANT_ID" -H "x-tenant-roles: iam-system" \
  -d "{\"tenant_id\":\"$TENANT_ID\",\"entry_type\":\"config.tenant_setting.changed\",\"action\":\"update\",
       \"actor\":{\"type\":\"user\",\"id\":\"$ADMIN_ID\",\"display\":\"admin@acme.example\"},
       \"target\":{\"type\":\"tenant_setting\",\"id\":\"mfa_freshness_seconds\"},
       \"occurred_at\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\",
       \"source_service\":\"iam-org-membership\",\"source_event_type\":\"TenantSettingChanged\",
       \"metadata\":{\"old\":900,\"new\":300}}"
# → 201; repeat with the same Idempotency-Key → 200 (idempotent replay)

# AL-1: query the tenant's trail as a tenant admin
curl -i "http://localhost:8080/api/v1/audit/events?entry_type=config.tenant_setting.changed&limit=10" \
  -H "x-user-id: $ADMIN_ID" -H "x-tenant-id: $TENANT_ID" -H "x-tenant-roles: tenant_admin"

# AL-3 then AL-4: export, then poll for the presigned download_url
curl -i -X POST http://localhost:8080/api/v1/audit/exports \
  -H "Content-Type: application/json" \
  -H "x-user-id: $ADMIN_ID" -H "x-tenant-id: $TENANT_ID" -H "x-tenant-roles: tenant_owner" \
  -d '{"entry_type":["config.tenant_setting.changed"]}'
curl -i http://localhost:8080/api/v1/audit/exports/<export_id> \
  -H "x-user-id: $ADMIN_ID" -H "x-tenant-id: $TENANT_ID" -H "x-tenant-roles: tenant_owner"

# AL-7: mesh-only provenance read, explicit tenant, no plan clamp
curl -i "http://localhost:8080/api/v1/internal/audit/events?tenant_id=$TENANT_ID" \
  -H "x-user-id: 00000000-0000-0000-0000-0000000000a1" \
  -H "x-tenant-id: $TENANT_ID" -H "x-tenant-roles: iam-system"
```

### Developer tools

There is **no Swagger UI route**. The OpenAPI contract is the static `docs/swagger/swagger.yaml`/`.json`, generated by `make swag`. Open it in any OpenAPI viewer. The AsyncAPI spec is served live:

```bash
curl -s http://localhost:8080/asyncapi.yaml | head
curl -s http://localhost:8080/asyncapi | jq '.channels | keys'
```

`DOCS_ENABLED` and `DOCS_AUTH_TOKEN` gate the AsyncAPI routes in production. Both routes are always mounted outside production.

---

## Testing domain events locally

This service only **consumes** events. Testing it locally means publishing an envelope to one of the producer topics that `init-floci.sh` creates, and watching the row land in `audit_events`.

### How the pipeline works

```
producer SNS publish (here: aws CLI inside floci)
    │
    ▼
SNS topic, e.g. iam-user-events ──(catch-all, RawMessageDelivery=true)──▶ SQS user-audit-q
                                                                                │
                           platform-events consumer (one per *-audit-q, SQS_CONCURRENCY)
                                                                                │
             Glue codec (only if `dataschema` is set) → Tier-1 metrics → BusIngest handler
                                                                                │
                                   ONE transaction: processed_events ledger insert (dedup on Envelope.ID)
                                                    + audit_events insert (ingest_mode='bus')
                                                    [+ redaction task, if UserDeleted]
                                                                                │
                        handler error → message left for redelivery → after 5 receives SQS redrives to user-audit-q-dlq
```

A redelivered envelope with the same `id` hits the ledger and is acknowledged without a second row (AL-INV-4). There is no schema-violation or other fast path to the DLQ. A message that cannot be stored (no `id`, a non-UUID `tenant_id`, or a Glue payload failing its schema) fails its handler and is redelivered. On the receive before `maxReceiveCount`, the dead-letter observer counts it in `platform_dlq_messages_total{reason="max_receive_exceeded"}`, and SQS then moves it to `<queue>-dlq`. Any DLQ depth above 0 pages (`platform_dlq_depth`, RB-1).

### Step 1: Start infrastructure

```bash
make docker-up
```

`scripts/init-floci.sh` runs automatically as floci's ready hook. It provisions:

- 11 SNS topics
- 11 `*-audit-q` queues, each with a `-dlq` and `maxReceiveCount=5`, subscribed catch-all with `RawMessageDelivery=true`
- the `iam-audit-archive` S3 bucket with Object Lock enabled
- 6 Glue registries (`iam-auth-events` … `iam-serviceaccount-events`), created last

The compose healthcheck polls that last registry, so `healthy` means fully provisioned. **No schemas are registered**: this service registers nothing. Plain-JSON envelopes skip the Glue codec entirely.

There is no floci web console in this compose file. Everything below uses the CLI.

### Step 2: Verify SNS, SQS, S3 and Glue exist

```bash
docker compose exec floci aws --region ap-south-1 sns list-topics
docker compose exec floci aws --region ap-south-1 sqs list-queues
docker compose exec floci aws --region ap-south-1 s3api get-object-lock-configuration --bucket iam-audit-archive
docker compose exec floci aws --region ap-south-1 glue list-registries
```

Every queue should be subscribed with no `FilterPolicy` (catch-all, AL-EVT-2):

```bash
docker compose exec floci aws --region ap-south-1 sns list-subscriptions \
  --query 'Subscriptions[].{Topic:TopicArn,Queue:Endpoint}' --output table
```

### Step 3: Publish a test envelope

With `make run` up (so the consumer fleet is polling), publish a `UserUpdated` on `iam-user-events`:

```bash
TENANT_ID=$(python3 -c 'import uuid; print(uuid.uuid4())')
EVENT_ID=$(python3 -c 'import uuid; print(uuid.uuid4())')
USER_ID=$(python3 -c 'import uuid; print(uuid.uuid4())')

docker compose exec floci aws --region ap-south-1 sns publish \
  --topic-arn arn:aws:sns:ap-south-1:000000000000:iam-user-events \
  --message "{\"id\":\"$EVENT_ID\",\"type\":\"UserUpdated\",\"source\":\"iam-user-profile\",
              \"specversion\":\"1.0\",\"time\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\",
              \"tenant_id\":\"$TENANT_ID\",\"actor\":\"$USER_ID\",\"subject\":\"$USER_ID\",
              \"data\":{\"user_id\":\"$USER_ID\"}}"
```

Publish the same message a second time to watch dedup in action. Swap the topic and type to exercise another queue: `iam-membership-events` with `DepartmentMembershipGranted`, or `tender-events` with any type (an unknown type becomes `tender.unknown`).

### Step 4: Inspect the row and the ledger

The local `postgres` superuser bypasses RLS, so no GUC is needed here:

```bash
docker compose exec postgres psql -U postgres -d audit -c \
  "SELECT entry_type, retention_tier, ingest_mode, source_topic, source_event_type, actor_type, actor_id
   FROM audit_events WHERE source_event_id = '$EVENT_ID';"

docker compose exec postgres psql -U postgres -d audit -c \
  "SELECT * FROM processed_events WHERE event_id = '$EVENT_ID';"
```

The expected result is exactly one `audit_events` row (`user.updated`, `ingest_mode = bus`, `source_topic = iam.user.events`), even after the duplicate publish.

### Step 5: Peek a DLQ

```bash
# Peek without deleting; the message becomes visible again after the visibility timeout.
docker compose exec floci aws --region ap-south-1 sqs receive-message \
  --queue-url http://floci:4566/000000000000/user-audit-q-dlq \
  --max-number-of-messages 10 --message-attribute-names All \
  | jq -r '.Messages[] | .Body | fromjson'
```

`RawMessageDelivery=true` means `Body` is already the envelope JSON, with no SNS wrapper to unwrap. For queue backlog without reading messages, use `sqs get-queue-attributes --attribute-names ApproximateNumberOfMessages`, or read `platform_queue_depth` and `platform_dlq_depth` on `:9090/metrics`.

### Troubleshooting events

| Symptom | Likely cause | Fix |
|---|---|---|
| No row appears | Server not running, or `*_AUDIT_QUEUE_URL` unset in `.env` (optional in dev, where the queue is then skipped) | `make run`; compare `.env` to the URLs `init-floci.sh` prints (`docker compose logs floci`) |
| `/readyz` returns `503` with `consumers: down` | floci not healthy yet, or a wrong `AWS_ENDPOINT_URL` | Wait for `docker compose ps` to show floci `healthy`; check `AWS_ENDPOINT_URL=http://localhost:4573` |
| The message ends up in `*-audit-q-dlq` after about 5 attempts | Envelope has no `id`, `tenant_id` is not a UUID, or a `dataschema`-framed payload fails its schema | Fix the envelope; see RB-1 in `docs/runbook.md` |
| The row has `entry_type = <domain>.unknown` | The type is not in the taxonomy for that topic (AL-EVT-4) | Expected for new types. Add it to `taxonomy.go` and `api/asyncapi.yaml`. |
| The row has a `_occurred_at_missing` / `_actor_unattributed` marker in `metadata` | The envelope had no `time`, or no resolvable actor | Send `time` and an `actor`/`actor_id` |
| Messages reappear after `receive-message` | Normal SQS visibility timeout, not a redelivery bug | Use `delete-message`, or let it expire |
| Stale local state across runs | `make docker-down` keeps the `pgdata` and `floci-data` named volumes | `docker compose down -v` for a clean slate |

---

## Testing

### Canonical tests (do not break)

- **`test/postgres/guc_test.go`**: `TestGUC_TransactionLocalNoLeakAcrossPooledBackend_ALINV3` and `TestGUC_ClearedAtCommit_ALINV3`. A tenant GUC must never survive `COMMIT` onto the next checkout of a pooled backend. These caught pgcommon's session-level GUC path, which is why the app pool forces `PGBouncerMode`.
- **`test/postgres/rls_test.go`**: the RLS cases. A missing or malformed GUC returns zero rows. A cross-tenant insert is rejected. The app role cannot UPDATE or DELETE (AL-INV-1). Platform-tenant rows are invisible to tenants (AL-D14).
- **`test/postgres/grants_test.go`**: `TestGrants_LeastPrivilegeMatrix_ALINV1`. CI's `check-grants.sh` mirrors it statically.
- **`test/postgres/archive_test.go`**: the drop gate. Drop is blocked while unverified (AL-INV-9) and on a count mismatch, it refuses while a redaction is pending (AL-INV-12), and a dropped month re-opens on late rows (D-19).
- **`test/integration/bus_ingest_test.go`**: `TestBus_PublishedEventPersistedOnceDespiteRedelivery_ALINV4_ALINV5` and `TestBus_SameRowShapeAsDirectWrite_ALINV2`, against real SNS and SQS on floci.
- **`test/unit/metrics_registry_test.go`**: `TestMetricRegistry_Conformance`, `…_VocabularyMatchesCode`, `…_ForbiddenLabelsAgree`, `…_ChecksAreGated`. They keep `registry.go`, the code and `deploy/monitoring/metric-registry.yaml` in lockstep. `TestAlerts_StaticMirrorMatchesHelm` keeps the static rule files and the Helm `PrometheusRule` identical.

### Coverage

`.github/scripts/coverage-gate.sh` reads the total from `go tool cover -func=coverage.out` and fails below `COVERAGE_THRESHOLD` (**95%**, set in `validate-test.yml`). Coverage is measured over `./cmd/...`, `./internal/...` and `./pkg/...` (`COVER_PKG_LIST`). It is merged across the unit, postgres and integration suites by `scripts/merge_coverage.py`. The e2e suite runs separately, without coverage or `-race`. The latest merged total is **96.5%**, as recorded in BUILD_PLAN and reproduced by the Phase 8 `make test-ci` runs.

---

## Environment variables

These come from `internal/config/config.go`, with defaults as coded. `.env-example` holds the local values. Library-read variables are marked.

| Variable | Default | Purpose |
|---|---|---|
| `APP_ENV` / `SERVICE_NAME` / `BUILD_VERSION` | `dev` / `iam-audit-log` / build ldflag | Env-gated behavior (`dev`, `development`, `local` and `test` relax checks). The reconciler appends `-reconciler` to `SERVICE_NAME`. |
| `APP_PORT` / `METRICS_PORT` | `8080` / `9090` | API listener / dedicated `/metrics` listener |
| `DATABASE_URL` | — | **Required.** `audit_app` DSN (via PgBouncer in production) |
| `DB_APP_ROLE` | `audit_app` | Role the server pool must authenticate as (no BYPASSRLS). Fatal outside dev, warning in dev. |
| `MIGRATION_DATABASE_URL` | — | **Required outside dev.** `audit_migrator`, direct to Postgres (not PgBouncer). Falls back to `DATABASE_URL` in dev. |
| `RECONCILER_DATABASE_URL` / `DB_RECONCILER_ROLE` | — / `audit_reconciler` | **Required for `cmd/reconciler`.** BYPASSRLS role, never given to the server |
| `PG_MAX_CONNS` / `PG_MIN_CONNS` / `PG_SLOW_QUERY_THRESHOLD` / `PG_MAX_CONN_LIFETIME` / `PG_MAX_CONN_IDLE_TIME` / `PG_HEALTH_CHECK_PERIOD` / `PG_SSLMODE` | library defaults (Helm: `20` server, `4` reconciler; `200ms`) | **pgcommon** `ConfigFromEnv` pool tunables |
| `PG_BOUNCER_MODE` | read by pgcommon | **Overridden to `true`** for both pools regardless (AL-INV-3) |
| `AWS_REGION` / `AWS_ENDPOINT_URL` / `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` | `ap-south-1` / — | floci override in dev (`http://localhost:4573`); IRSA in production |
| `GLUE_REGISTRY_REGION` | `ap-south-1` | Region for the decode-only Glue codec |
| `AUTH_AUDIT_QUEUE_URL` … `WF_TEMPLATE_AUDIT_QUEUE_URL` (11) | — | **Required outside dev**, one per inbound queue (LLD §7.1) |
| `SQS_MAX_RECEIVE_COUNT` / `SQS_VISIBILITY_TIMEOUT` / `SQS_CONCURRENCY` | `.env`: `5` / `30s` / `4` | **platform-events** `config.LoadSQS`. `SQS_MAX_RECEIVE_COUNT` also sets the dead-letter observe point (one short). |
| `AUDIT_ARCHIVE_BUCKET` / `AUDIT_ARCHIVE_KMS_KEY` | `iam-audit-archive` / `alias/iam-audit-archive` | Archive and export bucket, SSE-KMS key |
| `AUDIT_ARCHIVE_OBJECT_LOCK_MODE` | `COMPLIANCE` | `COMPLIANCE` or `GOVERNANCE`. Must be `COMPLIANCE` outside dev. |
| `AUDIT_HOT_WINDOW_DAYS` / `AUDIT_PRECREATE_MONTHS` / `AUDIT_WRITABLE_TRAILING_MONTHS` | `90` / `3` / `3` | Archival eligibility, partition look-ahead, and how many trailing months stay writable |
| `AUDIT_DEFAULT_QUERY_WINDOW_DAYS` | `365` | Plan-window fallback when the CAT-I2 map has no entry |
| `CATALOG_BASE_URL` | `http://iam-catalog-admin.iam.svc.cluster.local:8080` | CAT-I2 base URL; must be absolute `http(s)` |
| `CATALOG_PLANS_POLL_INTERVAL` / `CATALOG_PLANS_POLL_TIMEOUT` | `600s` / `3s` | Poller cadence. The timeout must be > 0 and shorter than the interval. |
| `MAX_INGEST_BATCH` / `MAX_METADATA_BYTES` | `500` / `8192` | AL-6 batch cap / metadata cap (compact bytes) |
| `INGEST_BATCH_RATE_LIMIT_RPS` / `INGEST_BATCH_RATE_LIMIT_BURST` | `10` / `20` | AL-6 per-tenant bucket (D-5) |
| `ARCHIVE_SYNC_MAX_ROWS` / `ARCHIVE_SYNC_MAX_BYTES` | `10000` / `52428800` | Archived-read sync bounds: above them AL-1 returns `202`, AL-7 returns `422` (D-10, D-14) |
| `EXPORT_SIGNED_URL_TTL` | `168h` | Export object lifetime |
| `EXPORT_DOWNLOAD_URL_TTL` | `15m` | Per-poll presigned URL TTL, at most `168h` (SigV4 limit) (D-11) |
| `EXPORT_RATE_LIMIT_PER_MINUTE` / `EXPORT_RATE_LIMIT_BURST` | `10` / `5` | AL-3 per-tenant bucket (§10.5) |
| `EXPORT_POLL_INTERVAL` / `EXPORT_JOB_LEASE` / `EXPORT_WORK_DIR` | `5s` / `15m` / `os.TempDir()` | In-server export worker. The lease must be between 1 m and 24 h and is heartbeated at lease/3 (D-2). |
| `OPS_STATS_INTERVAL` / `OPS_ARCHIVE_STALL_GRACE` / `OPS_REDACTION_PENDING_AGE` | `60s` / `48h` / `15m` | `OpsMonitor` gauges (D-21; RB-2, RB-7) |
| `RECONCILER_JOB` | — | Fallback for `--job` |
| `RECONCILER_TIMEOUT` | `30m` | Per-run deadline. The CronJob's `activeDeadlineSeconds` is 2100. |
| `PROCESSED_EVENTS_TTL_DAYS` / `PROCESSED_EVENTS_PRUNE_BATCH` | `8` / `10000` | Ledger retention (must exceed 7) / prune batch |
| `REDACTION_RETRY_MIN_AGE` / `REDACTION_RETRY_BATCH` | `5m` / `500` | `redaction-retry` job (batch 1–10000) |
| `REDACTION_SWEEP_WINDOW` | `2160h` | `redaction-sweep` look-back (24 h–9600 h) |
| `ARCHIVE_PART_MAX_ROWS` / `ARCHIVE_WORK_DIR` | `50000` / `os.TempDir()` | Rows per archive part (1000–1000000), temp dir |
| `DOCS_ENABLED` / `DOCS_AUTH_TOKEN` | `false` / — | AsyncAPI route gating in production |
| `OTEL_EXPORTER_OTLP_ENDPOINT` (+ `OTEL_EXPORTER_OTLP_INSECURE`, `OTEL_TRACES_SAMPLER_RATIO`, …) | unset (Helm: `otel-collector.observability.svc.cluster.local:4317`) | **gincommon** `InitTracingFromEnv`. Tracing is opt-in. |
| `SNS_TOPIC_ARN` / `OUTBOX_DATABASE_URL` | — | **Never read.** If set, the server logs a warning and ignores it (AL-INV-10). |

`LoadServer` and `LoadReconciler` report **every** missing or invalid variable at once and abort startup (fail-fast).

---

## Security

| Topic | Guidance |
|---|---|
| **Three-layer isolation** | RLS (`FORCE ROW LEVEL SECURITY`, tenant policy on `app.tenant_id`) is the DB floor. Identity comes only from gateway- or mesh-injected headers. Internal routes also require mesh mTLS, NetworkPolicy and the `iam-system` role (`RequireSystemRole`). |
| **AL-INV-3: GUC hygiene** | `app.tenant_id` is transaction-local on every checkout. The app pool forces `PGBouncerMode`, and CI rejects any non-`LOCAL` `SET app.tenant_id`. RLS violations are logged to `rls_violation_log` and exported as `iam_rls_violations_total`. |
| **AL-INV-1: append-only** | `audit_app` holds exactly INSERT+SELECT on `audit_events` (`check-grants.sh`, `TestGrants_LeastPrivilegeMatrix_ALINV1`). A trigger blocks UPDATE and DELETE even for privileged sessions. Rows leave only through the reconciler's gated partition drop or scoped redaction functions. |
| **Role separation** | Three DB roles, and each root gets only its own DSN. Helm splits secrets per root (`TestHelm_PerRootSecretsNeverShareRoleDSNs`). BYPASSRLS `audit_reconciler` is never mounted into the server. |
| **Immutable archive** | S3 Object Lock `COMPLIANCE` (enforced outside dev), SSE-KMS, sealed per-month manifest. A partition is dropped only after verification, through the in-database AL-INV-9 gate. |
| **GDPR (AL-INV-7/12)** | `UserDeleted` schedules a redaction task in the same transaction as the audit row and applies it immediately. `compliance_7y` rows are never redacted. Late rows for an erased subject are redacted before insert, and archival skips partitions with a pending redaction. |
| **Exports** | SSE-KMS objects, reached only through short-lived presigned URLs minted per AL-4 poll (default 15 m). No long-lived link is stored. |
| **Log hygiene** | Logs carry identifiers, never payloads or `metadata` (`TestLogHygiene_NoPayloadFields`). Metric labels exclude unbounded keys such as `tenant_id`, `user_id`, `actor_id` and `event_id` (registry `forbidden` list). |
| **Least-privilege AWS** | `deploy/iam/policy.json`, checked by `TestIAMPolicy_LeastPrivilege`: SQS receive, delete, visibility and attributes; S3 object, retention and Object Lock actions on the archive bucket; KMS `Decrypt`, `GenerateDataKey` and `DescribeKey`; Glue `GetSchemaVersion` and `GetSchemaByDefinition` only |
| **Pod hardening** | Distroless `nonroot` (UID 65532), `readOnlyRootFilesystem`, all capabilities dropped, `seccompProfile: RuntimeDefault`, NetworkPolicy enabled by default. The public ingress exposes only `/api/v1/audit` (`TestHelm_IngressExposesOnlyPublicAuditAPI`). |

---

## Observability

**SLOs** (`deploy/monitoring/slo-rules.yml`, multi-window burn-rate alerts):

| SLO | Objective |
|---|---|
| Ingest lag | 99% of rows ≤ 50 ms (HLD §3.4) |
| Event propagation | 99% ≤ 5 s |
| Query API availability | 99% of requests not 5xx |
| DLQ depth | `== 0`, always |

### Metrics

Metrics follow the **Enterprise Platform Observability Standard** (LLD §11 rev 0.26, gap 46), implemented in `internal/adapter/outbound/metrics/business.go`. Constant labels are injected centrally, never at a call site. The service label value is **`service="audit-log"`**. Every collector registers on `gincommon.MetricsRegisterer()`.

- **Tier 1 — `platform_*`** (carries `domain="iam"`, `service` and `environment`; all 10 are Canonical in the Platform Observability Registry, and this service adds none of its own):
  - `platform_messages_received_total{queue,event_type}`
  - `platform_messages_processed_total{event_type}`
  - `platform_messages_failed_total{event_type,reason}`
  - `platform_retry_total{event_type,reason}`
  - `platform_dlq_messages_total{queue,reason}`
  - `platform_duplicate_messages_total{event_type}`
  - `platform_dependency_request_seconds{dependency,operation,outcome}` (Catalog CAT-I2 and S3)
  - `platform_event_propagation_seconds{event_type}`
  - `platform_queue_depth{queue}`
  - `platform_dlq_depth{queue}`
- **Tier 2 — `iam_*`** (carries `service` and `environment`): `iam_rls_violations_total{violation_type}`
- **Tier 3 — `iam_audit_log_*`** (unique to this service):
  - Ingest: `events_ingested_total`, `duplicate_events_total`, `unknown_event_total`, `ingest_lag_seconds`, `directwrite_requests_total`, `dlq_messages_total`
  - Query and export: `query_window_clamped_total`, `query_archived_reads_total`, `export_jobs_total`
  - Storage and archive: `default_partition_rows`, `default_partition_rows_total`, `archive_stalled`, `archive_stalled_partitions`, `archive_lag_seconds`, `archive_partitions_total`, `retention_pruned_total`
  - Redaction: `redaction_pending_tasks`, `redaction_tasks_total`, `redaction_blocked_archive_total`
  - Catalog poller: `catalog_plans_poll_total`, `catalog_plans_stale_seconds`

The `registry.go` ledger and `deploy/monitoring/metric-registry.yaml` must agree. `TestMetricRegistry_*` checks this, and `metrics-registry-lint.sh` fails CI if a Proposed name is used as a live alert, recording-rule, SLO or HPA target. `check-metric-naming.sh` enforces tier prefixes, the `_total` and `_seconds` suffixes, and central labels. Library series come on top: gincommon `http_*`, pgcommon `pgmetrics` pool and query histograms, and platform-events `events_consumed_total`.

### Alerts, dashboards and runbooks

`deploy/monitoring/` holds four files:

- `app-alerts.yml`: 24 alerts
- `slo-rules.yml`: 6 burn-rate alerts
- `recording-rules.yml`: 20 `service:<metric>:<agg>` rules
- `dashboard-audit-log.json`: the Grafana dashboard

The Helm `PrometheusRule` (30 alerts) is **generated** from the three rule files by `scripts/gen-prometheusrule.py`. Every alert links a runbook in [`docs/runbook.md`](docs/runbook.md) (RB-1..RB-10).

### Tracing and logs

`gincommon.InitTracingFromEnv()` is gated on `OTEL_EXPORTER_OTLP_ENDPOINT`. pgcommon emits a span per query through the telemetry tracer. Each reconciler run is wrapped in a `reconciler.<job>` span. Logs are structured Zap through gincommon only (gap 43). They carry identifiers such as `tenant_id`, `event_type` and `job`, and never an event payload or metadata.

---

## Deployment

### Container image: two binaries

| Binary | Path in image | Purpose |
|---|---|---|
| `iam-audit-log-server` | `/iam-audit-log-server` | `cmd/server`, the image's `ENTRYPOINT`. Serves AL-1..AL-7 and runs the 11-queue consumer fleet, the CAT-I2 poller, the export worker and `OpsMonitor`. Self-migrates at startup. |
| `iam-audit-log-reconciler` | `/iam-audit-log-reconciler` | `cmd/reconciler`, dispatched with `--job=<name>` by the 4 CronJobs. No listener. |

The `Dockerfile` has two stages:

- **Builder:** `golang:1.26.6-bookworm`, pinned by SHA digest. The private-module token is passed as a BuildKit secret and never lands in a layer.
- **Runtime:** `gcr.io/distroless/static-debian12:nonroot`, also digest-pinned, with no shell and running as non-root. Only the two binaries are copied in.

The image has `EXPOSE 8080 9090`.

### Helm chart

`deploy/helm/` renders one `Deployment` and the 4 `CronJob`s from the same image. Key settings:

| Setting | Value |
|---|---|
| Replicas | `replicaCount: 3` |
| HPA | Off by default (`autoscaling.enabled: false`). When enabled: 3–6 replicas, CPU 70% / memory 80%, optionally scaling on `targetQueueDepthPerReplica` via `platform_queue_depth`. |
| PDB | `minAvailable: 2` |
| Resources | CPU `250m`/`1`, memory `256Mi`/`512Mi` |
| `terminationGracePeriodSeconds` | `45` |
| Probes | `startupProbe` on `/healthz` with a 120 s budget for migrations. Liveness on `/healthz`, readiness on `/readyz`. |
| Monitoring | `ServiceMonitor` (30 s), `PrometheusRule` |
| Network and ingress | NetworkPolicy. HTTPRoute or Ingress is off by default and exposes only `/api/v1/audit`. |

Secrets are split per root: `DATABASE_URL` and `MIGRATION_DATABASE_URL` for the server, `RECONCILER_DATABASE_URL` for the reconciler.

### Migration safety

`cmd/server` runs `internal/adapter/outbound/postgres/migrations/` at startup through pgcommon's migrate runner, as `audit_migrator` on a direct connection. The runner takes an advisory lock, so migrations must not go through PgBouncer. There are 10 up/down pairs: `000001_schema`, `000002_rls`, `000003_roles`, `000004_triggers`, `000005_partition_bootstrap`, `000006_archive_manifest_export_claim`, `000007_redaction`, `000008_reconciler`, `000009_ops_stats` and `000010_rls_violation_export`. Prerequisites outside this repo (Terraform, the Object Lock bucket, KMS, IRSA, the queues) are listed in [`docs/implementation/RELEASE_CHECKLIST.md`](docs/implementation/RELEASE_CHECKLIST.md).

---

## CI

Five workflow files:

- **`ci.yml`**: the orchestrator. It runs `validate-test.yml` and `validate-quality.yml` in parallel with `build-image`, which does Hadolint → Buildx cached build → Trivy CVE scan (plus SARIF and a CycloneDX SBOM) → smoke tests for both binaries (200 MB size limit plus a startup check). It also posts a PR summary. On push to `main` it pushes to GHCR.
- **`validate-test.yml`** (reusable):
  1. `make test-ci` (unit, postgres/RLS and integration, with `-race` and merged coverage)
  2. coverage gate (**95%**)
  3. `make test-e2e`
  4. `arch-lint.sh`
  5. Swagger staleness check
  6. event-schema sync check (AsyncAPI → JSON)
- **`validate-quality.yml`** (reusable):
  1. `go mod verify`
  2. reject HTML-escaped operators in workflows
  3. RLS-6 non-`LOCAL` `SET app.tenant_id` grep
  4. `check-metric-naming.sh`
  5. `check-observability-confinement.sh`
  6. `metrics-registry-lint.sh`
  7. `check-forbidden-events-bypass.sh` and `check-outbox-access.sh` (AL-INV-10)
  8. `check-asyncapi-receive-only.sh`
  9. `check-grants.sh` (AL-INV-1)
  10. `gofmt`, `go mod tidy` drift, `go vet`, `golangci-lint`, `govulncheck`
  11. Dockerfile base-image digest pin check
- **`changelog-check.yml`**: fails a PR that touches `internal/`, `api/` or `deploy/` without updating `CHANGELOG.md`.
- **`release.yml`**: a tag-triggered pipeline:
  1. re-validate
  2. verify the tag and the CHANGELOG entry
  3. cross-compile the binaries
  4. Docker build and push, Trivy scan and SBOM
  5. SLSA provenance and Cosign sign/verify
  6. an optional Helm deploy with a post-deploy health gate

There are no schema-registry workflows because this service registers no schemas.

**Required GitHub Actions repository secret: `GO_PRIVATE_TOKEN`.** Every workflow that downloads modules needs it to fetch the private `platform-events`, `platform-gincommon` and `platform-pgcommon` modules.

---

## Docker

### What the bundled `docker-compose.yml` starts

| Container | Image | Host port(s) | Purpose |
|---|---|---|---|
| `app` | built from the local `Dockerfile` (needs the `GO_PRIVATE_TOKEN` env for the build secret) | `8080`, `9090` | This service, when run with `docker compose up` instead of `make run` |
| `postgres` | `postgres:15-alpine` | `5548 → 5432` | The `audit` database (user and password `postgres`) |
| `floci` | `floci/floci:2.1.0-compat` | `4573 → 4566` | SNS, SQS, S3 and Glue emulator, provisioned by `scripts/init-floci.sh` |

`make docker-up` starts only `postgres` and `floci`, not `app`, so local development normally runs the service with `make run`. There is no PgBouncer, cache or floci UI container. Host ports are deliberately offset: `5544` and `4566`–`4572` are used by sibling stacks.

### Building the service image

```bash
docker build -t iam-audit-log:local --secret id=go_private_token,src=<(echo "$GO_PRIVATE_TOKEN") .
```

### Health and readiness

| Endpoint | Returns | Checks |
|---|---|---|
| `GET /healthz` | `200 {"status":"ok"}` | Pure liveness. It never inspects a dependency. |
| `GET /readyz` | `200 {"status":"ready","checks":{…}}` / `503 {"status":"not ready",…}` | `database` (pgcommon pool health) and `consumers` (the SQS consumer fleet), run concurrently |

### Minimum required environment variables

```bash
# PostgreSQL (three roles; server and reconciler each get only their own)
DATABASE_URL=postgres://audit_app:...@pgbouncer:6432/audit?sslmode=require
MIGRATION_DATABASE_URL=postgres://audit_migrator:...@audit-db:5432/audit?sslmode=require
RECONCILER_DATABASE_URL=postgres://audit_reconciler:...@pgbouncer:6432/audit?sslmode=require   # cmd/reconciler only

# Inbound queues (all 11 required outside dev)
AUTH_AUDIT_QUEUE_URL=https://sqs.ap-south-1.amazonaws.com/<acct>/auth-audit-q
# … USER_ / MEMBERSHIP_ / TENANT_ / DELEGATION_ / SERVICEACCOUNT_ / TENDER_ / BILLING_ / USAGE_ /
#   WF_WORKFLOW_ / WF_TEMPLATE_AUDIT_QUEUE_URL

# Archive
AUDIT_ARCHIVE_BUCKET=iam-audit-archive
AUDIT_ARCHIVE_KMS_KEY=alias/iam-audit-archive
AUDIT_ARCHIVE_OBJECT_LOCK_MODE=COMPLIANCE

# CAT-I2
CATALOG_BASE_URL=http://iam-catalog-admin.iam.svc.cluster.local:8080
```

---

## Cross-service dependencies

### 1. Synchronous outbound calls (this service → other services)

| Operation | Dependency | Adapter package | Posture | On failure |
|---|---|---|---|---|
| Plan-window map (CAT-I2 poll) | Catalog / Admin Config | `outbound/catalog/` | stale-if-error | Keeps the last good map; unknown plans use `AUDIT_DEFAULT_QUERY_WINDOW_DAYS`. `iam_audit_log_catalog_plans_stale_seconds` grows. |
| Archived reads (AL-1/AL-2/AL-7) | AWS S3 | `outbound/s3/` | fail-closed | `503 dependency_unavailable`. A partial trail is never returned as complete. |
| Export write and presign (AL-3/AL-4 worker) | AWS S3 + KMS | `outbound/s3/` | fail the job | The job is marked `failed` with a reason, and AL-4 reports it. A worker that dies mid-job loses its lease, and another replica reclaims the job. |
| Archive, verify and drop (reconciler) | AWS S3 + KMS | `outbound/s3/` | fail-safe | The partition is not dropped (AL-INV-9) and the job exits non-zero. `archive_stalled` fires after `OPS_ARCHIVE_STALL_GRACE`. |
| Glue schema lookup (bus decode, only for envelopes with `dataschema`) | AWS Glue | `outbound/glue/` | redeliver | The handler fails, so SQS redelivers the message and redrives it to the DLQ after 5 receives |

Hot-path writes (AL-5/AL-6 and bus ingest) depend on **Postgres only**.

### 2. Inbound callers (other services → this service)

| Caller | Endpoints used | Purpose |
|---|---|---|
| **Catalog / Admin Config** | AL-5 | `config.department.*`, `config.plan.updated` (platform tenant) |
| **Group Mapping / JIT Config** | AL-6 | `config.group_mapping.*` reconciles |
| **Tender ACL** | AL-5 / AL-6 | `config.tender_acl.*` grants, revokes and cascades |
| **Org & Membership** | AL-5 | `config.tenant_setting.changed`, `invitation.*` |
| **Realm Provisioner** | AL-5 | `config.idp.changed` |
| **Operator tooling** | AL-5 | `security.cross_tenant_access` |
| **Product UI (via gateway)** | AL-1..AL-4 | The tenant admin's "View audit log" and exports |
| **Compliance / verification services** | AL-7 | Provenance reads beyond the plan window |

### 3. Async event dependencies

**Events this service consumes:** all 11 queues in [Integrating §2](#2-bus-producers-publish-to-your-own-topic-and-nothing-else-is-needed). Each is catch-all, so every type on the topic is recorded. Two event families also drive side effects beyond the audit row:

| Event | Queue | Side effect |
|---|---|---|
| `UserDeleted` | `user-audit-q` | Schedules and applies GDPR redaction for that subject (AL-INV-12) |
| Billing plan and subscription lifecycle | `billing-audit-q` | Projects the tenant's plan into `tenant_plan_window`, guarded against out-of-order delivery (`TestPlanWindow_RecencyGuard_ALEVT3`) |

**Events this service produces:** none (AL-INV-10).

### 4. Infrastructure dependencies

| System | Role |
|---|---|
| **PostgreSQL** (`audit` DB) | The trail, the ledger, the archive state, export jobs and redaction tasks. RLS, monthly partitions, three roles. |
| **AWS SQS** | 11 `*-audit-q` queues, each with a `-dlq` (`maxReceiveCount=5`) |
| **AWS SNS** | Producer-owned topics. This service only holds the catch-all subscriptions. |
| **AWS S3 + KMS** | `iam-audit-archive`: Object Lock COMPLIANCE archive and export objects, SSE-KMS `alias/iam-audit-archive` |
| **AWS Glue Schema Registry** | 6 IAM registries, read-only, used only for envelopes that carry `dataschema` |

---

## Out of scope

| Concern | Where it lives |
|---|---|
| Publishing domain events, outbox | Nowhere here, by design (AL-INV-10) |
| The business events and their schemas | Each producer's own topic and Glue registry |
| Plan catalog, `audit_query_window_days` | Catalog / Admin Config Service |
| Tenant, membership and role state | Org & Membership |
| User identity, profile, erasure itself | User Profile (this service only redacts on `UserDeleted`) |
| Authentication, JWT issuance and validation | Keycloak and the gateway |
| Tender ACLs, delegation, group mapping | Their own services (they write *audit entries* here) |
| SIEM forwarding, cross-tenant analytics | Not provided. The trail is tenant-scoped by RLS. |
| Caching | None, by design (LLD §6) |

---

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for development setup, extending the taxonomy and the service, testing requirements and the PR checklist.

| Document | Description |
|---|---|
| [`.claude/CLAUDE.md`](.claude/CLAUDE.md) | Top-level guidance for Claude Code in this repo |
| [`.claude/architecture.md`](.claude/architecture.md) | Package layout, composition roots, shared-library confinement, dependency rules |
| [`.claude/database-schema.md`](.claude/database-schema.md) | Tables, partitions, roles, RLS, triggers, in-database functions |
| [`.claude/api-caching-events.md`](.claude/api-caching-events.md) | Endpoint catalogue, consumed-event catalogue, status codes |
| [`.claude/request-flows.md`](.claude/request-flows.md) | Ingest, query routing, export, archival, redaction flows |
| [`.claude/operations.md`](.claude/operations.md) | Security, observability, configuration, CI/CD, degradation |
| [`ARCHITECTURE.md`](ARCHITECTURE.md) | Detailed architecture narrative |
| [`docs/architecture/`](docs/architecture/README.md) | Mermaid diagrams |
| [`docs/lld/iam-lld-audit-log-service.md`](docs/lld/iam-lld-audit-log-service.md) | The LLD: §5 API, §7 events, §11 observability, §17 error taxonomy, §22 decisions |
| [`docs/implementation/BUILD_PLAN.md`](docs/implementation/BUILD_PLAN.md) | Phase plan, invariant → test map, D-1..D-21, gaps |
| [`docs/implementation/RELEASE_CHECKLIST.md`](docs/implementation/RELEASE_CHECKLIST.md) | Deployment prerequisites outside this repo |
| [`docs/runbook.md`](docs/runbook.md) | RB-1..RB-10 |
| [`deploy/monitoring/README.md`](deploy/monitoring/README.md) | Metric inventory, rule files, dashboard |
| [`CHANGELOG.md`](CHANGELOG.md) / [`VERSIONING.md`](VERSIONING.md) | Release history / versioning policy |

---

## License / ownership

BCBP Solutions FZC LLC — internal platform service. Not for external distribution.
