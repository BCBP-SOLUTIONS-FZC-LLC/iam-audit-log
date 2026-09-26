# iam-audit-log — Build Plan & Trace

Spec: `docs/lld/iam-lld-audit-log-service.md` (rev 0.20). This file is the running
trace: every item cites the LLD section / ID it implements. The LLD is not edited;
spec gaps are listed in §C for a doc change.

Conventions ground truth (surveyed 2026-09-25):
- **iam-realm-provisioner (RP)** — two composition roots, pools, migrations, Helm, CI, Dockerfile.
- **iam-authz-enrichment (AE)** — pure-consumer wiring; publisher never constructed.
- **iam-org-membership / iam-delegation** — standalone `check-forbidden-events-bypass.sh`, `check-metric-naming.sh`, canonical RLS test suite.
- **iam-event-consumer** — reference Glue decode codec + validating codec.

Pinned (from sibling `go.mod`, matches LLD §3.1): Go `1.26.6`, `platform-events v1.4.0`,
`platform-gincommon v1.3.0`, `platform-pgcommon v1.3.0`, `pgx/v5 v5.10.0`, `gin v1.12.0`,
`testcontainers-go v0.44.0`, `santhosh-tekuri/jsonschema/v6`, `aws-sdk-go-v2` (s3, sqs, glue).

---

## A. Phase checklist

### Phase 0 — Scaffolding (§3, §3.2, §3.3, §12, §13) — ✅ done, awaiting review
- [x] Layout per §3 + RP mechanics (`cmd/{server,reconciler}`, `cmd/reconciler/jobs`, `internal/core/{domain,port,service}`, `internal/adapter/{inbound,outbound}/*`, `internal/eventschema`, `internal/config`, `pkg/requestctx`, `api/`).
- [x] Config (`internal/config`, AE convention): every §12 var with LLD defaults; fail-fast with all problems listed; production requires `MIGRATION_DATABASE_URL`, all 11 queue URLs, and Object Lock `COMPLIANCE`.
- [x] Pools: server = `DATABASE_URL`/`audit_app` with `GUCProvider` bound; reconciler = `RECONCILER_DATABASE_URL`/`audit_reconciler`, no GUC, PgBouncer mode. Startup role assertion (`current_user` + `rolbypassrls`) is fatal outside dev.
- [x] Migration runner (`pgmigrate.Runner`, embedded dir, `pgcommon_migrations` table); an empty set is a no-op. **No `outbox.ApplySchema`.**
- [x] `/healthz`, `/readyz` (concurrent checks; `database` now, consumers in Phase 3), `/metrics` on `METRICS_PORT`; `/asyncapi`, `/asyncapi.yaml` (gated in production).
- [x] Middleware: `IdentityBridgeMiddleware` (401 `missing_identity_headers`; binds the transaction-local GUC), `RequireSystemRole` (403 `forbidden_peer`), `RequireAuditReader` (403 `insufficient_permissions`), `RequireIdempotencyKey` (400, required, ≤256 chars), `RequireJSONContentType`.
- [x] Shutdown order per RP; `run() int` in both roots so defers always run.
- [x] AL-INV-10: `SNS_TOPIC_ARN`/`OUTBOX_DATABASE_URL` produce a warning only. `LoadSNS`/`LoadOutbox` are never called (CI-forbidden).
- [x] CI: `.go-arch-lint.yml` (`depOnAnyVendor: false`; domain/service vendor-free, verified by a probe), `arch-lint.sh`, `check-grants.sh`, `check-forbidden-events-bypass.sh` (probe-verified), `check-asyncapi-receive-only.sh`, `check-metric-naming.sh`, workflows (`validate-test` gains the invariant gates; release `deploy-gate` runs under `production-data-migrations`), Makefile, `.golangci.yml` (0 issues), Dockerfile (both binaries), docker-compose (PG15 + floci), `scripts/init-floci.sh`.
- [x] Helm: 3 replicas, required pod labels, **separate server/reconciler Secrets** (neither pod can read the other role's DSN), CronJobs disabled until their jobs exist, NetworkPolicy (gateway + same-namespace ingress, AWS/PG/Catalog egress), `deploy/iam/policy.json` (no `sns:*`; explicit Deny on S3 delete / lock bypass).
- [x] Tests: config defaults/queues/role separation, router + middleware (401/403/GUC binding/idempotency key/docs gating), §17 status map, role checks, `TestAsyncAPI_NoSendOperations_ALINV10`, composition-root boot/serve/drain against PG15 (testcontainers), smoke (non-zero exit on missing config).

- [x] **`.github/` aligned with iam-org-membership (2026-09-26, user request).** O&M's workflows and scripts are the base. Only these deltas were applied: two binaries, the build secret, the `production-data-migrations` deploy environment, per-root Helm Secrets, the `iam` namespace, and a coverage gate now at 95% (96.8% measured). The AL-INV gates were added to `validate-quality`.
  - **Not ported:** `schema-registry.yml`, `schema-prune.yml`, `schema-health-quarterly.yml`, `freeze-watchdog.yml`, `stage-produced-event-schemas.sh`. They all manage schemas the service *produces*, and this service produces and registers none (LLD §7.3.1, AL-INV-10).
  - **Kept this repo's stricter versions:** `arch-lint.sh` (O&M's runs `go-arch-lint` only), `check-forbidden-events-bypass.sh` (a superset of O&M's three checks), `smoke-tests.sh` / `prepare-release-binary.sh` / `create-github-release.sh` (two-binary), `.golangci.yml`.

- [x] **Test tiers mirroring iam-org-membership/test (2026-09-26).** `test/{dbseed,fixtures,unit,postgres,integration,e2e}` in place. Phase 1+ extend the same harnesses: the RLS Cases 1–4 and partition suite go in `test/postgres`, and the consumer, archive and redaction suites go in `test/integration`. **The postgres tier found a real AL-INV-3 defect, now fixed:** pgcommon binds GUCs session-level unless `PGBouncerMode` is on, so the app pool now forces it on.

### Phase 1 — Schema & migrations (§4.1–§4.6, §19) — ✅ done, awaiting review
- [x] `000001_schema` — pgcrypto, the 6 enums (§4.1), `audit_events` monthly-partitioned + `audit_events_default`, `audit_event_archive_state`, `processed_events`, `tenant_plan_window`, `audit_export_jobs` (+`updated_at`, gap 9), `audit_redaction_tasks`, and every §4.2 index and CHECK.
- [x] `000002_rls` — ENABLE/FORCE/REVOKE, `app_tenant_id()`, `rls_check_tenant()`, the 1%-sampled `log_rls_violation()`, `rls_violation_log`, and `tenant_isolation` USING + WITH CHECK on `audit_events` / `audit_export_jobs`. EXECUTE on the definer functions is revoked from PUBLIC.
- [x] `000003_roles` — `audit_app` / `audit_reconciler` / `audit_migrator` / `admin_readonly`, in RP's idempotent style. Roles are created NOLOGIN only if missing, RLS attributes are re-asserted, and the least-privilege matrix is attached. `audit_app` = exactly INSERT+SELECT on `audit_events`, plus the D-1/D-2 grants.
- [x] `000004_triggers` — `forbid_audit_mutation()` BEFORE UPDATE/DELETE (cloned onto every partition); `touch_row()` on `audit_export_jobs`.
- [x] `000005_partition_bootstrap` — `audit_ensure_partitions(ahead, trailing)` SECURITY DEFINER (see gap 26). The bootstrap creates the current month ±3; the down step drops only EMPTY partitions.
- [x] `PartitionService` (core) + `PartitionRepository` (adapter). It runs at server startup, where a failure is non-fatal because DEFAULT still catches rows, and as step 1 of the `reconcile` CronJob (now enabled in Helm).
- [x] Every migration has a `.down.sql`, verified by a full down/up round trip. The deploy runs under `production-data-migrations` (release.yml).
- [x] Tests (§14 Postgres rows):
  - RLS Cases 1–4 on `audit_events`, plus the tenant-scoping, `platform_tenant` and export-jobs RLS tests.
  - The trigger blocks privileged sessions; the reconciler may DELETE and has no UPDATE.
  - The live grant matrix; partitions are not directly accessible to `audit_app`; function EXECUTE is not granted to PUBLIC; role attributes.
  - CHECKs, the dedup backstop, the ledger, and redaction-task idempotency.
  - Partition bounds (UTC), routing, the DEFAULT catch, both roles, bounds, RB-3 skip, concurrent creators, and new partitions inheriting RLS + triggers.
  - Migrations: round trip, the down step keeps data, and no outbox.
  - Unit: `PartitionService`, the `reconcile` job, `PartitionName`. e2e: the reconciler binary runs `reconcile` as `audit_reconciler`.
  - **Deferred to Phase 7 by the LLD's own split:** the drop-after-`verified` gate and late-arrival re-open, which need the archive state machine.

### Phase 2 — Ingest core + AL-5/AL-6 (§4.1, §5.4, §7.1, §10.2, §10.3, §17; AL-D1, AL-D3, AL-D11, AL-D14) — ✅ done, awaiting review
- [x] `domain/taxonomy.go` — the full §7.1 table as a compiled map (72 entry types; 57 bus rows incl. dual-source `MFAReset` / `TenantReactivated`; 16 direct-write rows). Also `ClassifyBus` with the `<domain>.unknown` / `security_3y` fail-safe (AL-EVT-4, for Phase 3) and `ValidEntryTypeShape`. A `test/unit` contract test parses §25 straight from the LLD, and a domain test transcribes the §7.1 table row by row.
- [x] `domain/actor.go` — the §10.3 actor model. `anonymous` ⇔ no id (AL-D11: a claimed identity is never promoted to `actor_id`); `iam_system` ⇒ the `…00a1` sentinel; other types need a UUID. `platform_tenant` (AL-D14) is an ordinary tenant id: accepted, bound, checked.
- [x] `domain/entry.go` — `AuditEntry` (the single row shape, AL-INV-2) and `BuildDirectWriteEntry`, the §5.4 validation. The tier is always derived from `entry_type`; a caller tier is ignored (AL-INV-6/11). Metadata is an object, compacted and capped at `MAX_METADATA_BYTES`. §17 codes throughout.
- [x] `IngestService` — `DirectWrite` and `DirectWriteBatch` (per-entry, partial success; stops early when the DB is down). This is the same `AuditWriter.Append` the Phase 3 fleet will use.
- [x] `AuditRepository.Append` — one tx bound to the **body** tenant: ledger insert → `INSERT … ON CONFLICT (source_event_id, occurred_at) DO NOTHING` → replay lookup under RLS. A key already used by another tenant gives `400` and never leaks that tenant's row. Metadata is bound as a string (gap 28).
- [x] HTTP — AL-5 (`201`/`200`) and AL-6 (always `207`, D-4) at the literal LLD paths. `Idempotency-Key` is required, `RequireSystemRole` → `403 forbidden_peer`, the per-tenant limiter gives `429` (D-5), and `trace_id` defaults to the traceparent. Swagger regenerated.
- [x] Tests (§14 Ingest row):
  - unit: taxonomy completeness vs the LLD, the actor matrix, the validation matrix, the metadata cap, the service and the handlers.
  - postgres: create/replay, the ledger-prune backstop, cross-tenant key reuse, body-tenant binding, `platform_tenant` and anonymous actors, and a concurrent same-key race.
  - e2e: the 201→200 replay, caller tier ignored, the traceparent default, the §17 rejection matrix, cross-tenant forgery, batch partial success, `batch_too_large` and `429`.
  - **Producer contract tests** replaying realm-provisioner's and Catalog's shipped AL-5 client shapes.
- Actor derivation from **bus** envelopes (the `ip_address:"system"` → `iam_system` mapping, §10.3) belongs with the per-source normalisers in Phase 3.

### Phase 3 — Bus consumers (§7.1–§7.3, §7.5, §7.6, §9; AL-D9, AL-D12) — ✅ done, awaiting review
- [x] **Producer survey** of the six IAM producers plus the external-domain docs. It shaped D-7/D-8/D-9: five different "system" actor spellings; auth's sentinel actor with the real user in `subject`; RP events with no actor; `ip_address:"system"` from delegation and O&M; `plan` as the only plan field; workflow wire names differing from the LLD.
- [x] `outbound/glue` — a decode-only codec: 18-byte header, zlib-safe, bomb-bounded. Every payload is validated against the **producer's registered schema version**, resolved with `glue:GetSchemaVersion` and compiled once per version. An unresolvable version or a violation is a decode failure → redelivery → DLQ. It is invoked only when `dataschema` is set (AL-D12; platform-events' own gate). `Encode` refuses (AL-INV-10).
- [x] `domain/bus.go` — `BuildBusEntry` implements D-8 (actor/target), D-9 (truncation marker), the `<domain>.unknown` fail-safe, provenance (AL-INV-5), missing time / invalid IP markers, and wrapping of non-object payloads. An unstorable tenant_id is an error → DLQ.
- [x] `IngestService.IngestBus` — the **same** `AuditWriter.Append` as AL-5 (AL-INV-2), with the queue discriminator. The `tenant_plan_window` projection comes from TenantCreated / TrialStarted / TenantConverted / TenantPlanChanged (`plan`), with the window from CAT-I2 when known (Phase 5), else `AUDIT_DEFAULT_QUERY_WINDOW_DAYS`. `PlanWindowRepository` applies the recency guard (AL-EVT-3).
- [x] `inbound/consumer` — `Fleet` builds one `NewSQSConsumerWithClient` per configured queue, with concurrency 4, `SQS_VISIBILITY_TIMEOUT`, and RP's DLQ pattern (observe at receive 4, return an error, let SQS redrive at 5). It carries Tier-1 metrics and a `/readyz` "consumers" check, and shutdown drains in-flight handlers before the pool closes.
- [x] `api/asyncapi.yaml` — 65 receive-only messages (the §7.1 rows plus the D-7 aliases), each carrying `x-audit-entry-type` / `x-audit-retention-tier`, with a contract test against the compiled taxonomy.
- [x] Tests:
  - unit: codec (registry validation, caching, zlib, bombs, header errors), the normaliser matrix built from real producer shapes, bus ingest + projection, fleet/handler/DLQ/metrics.
  - postgres: plan-window recency guard.
  - integration (floci + PG15, the real fleet): publish → row with provenance; redelivery dedup; unknown persisted; D-7 alias; plan projection; a **Glue-framed payload validated against a schema registered in floci**; a schema violation → DLQ and never persisted; bus/direct-write same shape.
- Deferred by design: the `iam_audit_log_unknown_event_total` / DLQ-depth Tier-3 metrics and alerts go to Phase 8, and the `UserDeleted` → redaction task to Phase 6.

### Phase 4 — Query + export (§5.1–§5.4 AL-1..AL-4, §8.4, §15.3; AL-INV-8, AL-D5; decisions D-2, D-10..D-12) — ✅ done, awaiting review
- [x] `RequireAuditReader` (`tenant_admin` ∨ `tenant_owner`) → `403 insufficient_permissions` on AL-1..AL-4. Tenant and caller always come from the gateway headers, never from the query or body.
- [x] Window resolution (§5.4) in `domain.ResolveWindowDays`: the live CAT-I2 map for the row's `plan_code` (nil until Phase 5), then `row.query_window_days`, then `AUDIT_DEFAULT_QUERY_WINDOW_DAYS` when there is no row. `ClampWindow` sets `window_clamped`/`effective_from`. A range entirely before the window is `200` with an empty page. AL-2 outside the window → `404`.
- [x] Keyset on `(occurred_at DESC, id DESC)` with an opaque base64url cursor; a malformed cursor → `400`. AL-2 is an RLS-scoped hot read, with an archive fallback that reads only the objects whose id range holds the id (gap 32).
- [x] Hot/archived routing by **partition existence** (D-12), not a fixed 90-day cut. `audit_archive_objects` is the per-object manifest (`000006`, gap 29). An archived read streams the tenant's own objects (D-10 keys) and merges them with hot rows under one keyset. If the manifest estimate exceeds `ARCHIVE_SYNC_MAX_ROWS`/`_BYTES`, an AL-3 job is auto-created and the read returns `202`, drawing on the AL-3 rate limit. A missing object is skipped with a warning; any other S3 failure → `503`.
- [x] AL-3/AL-4: the filter is clamped at request time and stored clamped (AL-INV-8). The worker runs in `cmd/server` as `audit_app` (D-2): `claim_export_job(lease)` is SECURITY DEFINER with SKIP LOCKED, re-claims when a lease expires, and heartbeats at lease/3. Output is gzipped JSONL assembled in a temp file and uploaded to `exports/{tenant}/{id}.jsonl.gz` with SSE-KMS (omitted only against the emulator). Retrieval lasts 7 days, and each AL-4 poll gets a fresh `EXPORT_DOWNLOAD_URL_TTL` URL (D-11). A lapsed job is marked `expired` lazily. A failure stores only the §17 code or `internal_error`. AL-3 has a per-tenant limit → `429` + `Retry-After`. There is no dedup (gap 31).
- [x] No cache (AL-D5). New env vars are in gap 33, `.env-example` and Helm `serverEnv`; swagger is regenerated.
- [x] Tests:
  - unit: filter/cursor/clamp/precedence tables, the query service (merge, cursor across sources, D-10 deferral on both bounds, missing object) and the export service (the worker end to end on a gzip object, failure codes, presign TTL cap, lapse), handlers through `NewRouter`, and the S3 store (error classification, SSE-KMS on/off, offline presign). Includes `TestQuery_WindowClampIndependentOfTier_ALINV8`.
  - postgres: keyset order and ties, stability under concurrent ingest, every filter, `TestQuery_TenantIsolation_ALINV3`, `TestQuery_ArchivedObjectsOnlyForDroppedPartitions_D12`, manifest grants/RLS, export CRUD, `TestExport_ClaimOldestAcrossTenants_D2`, concurrent distinct claims, lease re-claim and bounds, state transitions, and the `claim_export_job` grants/owner.
  - e2e (floci S3): AL-5 → AL-1/AL-2, pagination with a concurrent insert, tenant isolation, `member` → 403, `TestAL1AL2_PlanWindowClamp_ALINV8`, the bad-request matrix, and `TestAL3AL4_ExportToSignedURLContents` (presigned GET → gunzip → exactly the tenant's rows).
  - Merged coverage is 96.1%.
- Deferred by design: the live plan map (Phase 5 poller), manifest writes and Object Lock (Phase 7 archiver), and query/export metrics (Phase 8).

### Phase 5 — CAT-I2 poller + AL-7 (§4.2, §12, AL-D15; §5.2/§5.3 AL-7; decisions D-13, D-14) — ✅ done, awaiting review
- [x] `outbound/catalog` client: `GET {CATALOG_BASE_URL}/api/v1/internal/plans` with headers `x-user-id: iam-system`, `x-tenant-id: 00000000-…0000` and `x-tenant-roles: iam-system`. That matches Catalog's `RequireSystemRole` and the O&M client byte for byte. Per-request `CATALOG_PLANS_POLL_TIMEOUT`, a 1 MiB body cap (oversize → error), non-200 → error, and timeouts marked `port.ErrCatalogTimeout`. It decodes only `plans[].{code, audit_query_window_days}` and `record_versions`.
- [x] `service.PlanPoller` (implements `port.PlanWindows`): polls once at startup, then every `CATALOG_PLANS_POLL_INTERVAL`. The map is an atomic snapshot, swapped only when `record_versions` differs. **Stale-if-error:** any failure keeps the last good map. A response with no plans, a non-positive window, or a plan missing from `record_versions` is rejected whole, so a malformed response can never replace a good map. Until the first success, `WindowDays` reports no entry and callers fall back per D-13. The same poller feeds the bus plan projection (Phase 3) and the query window (Phase 4). Config validates the base URL and requires timeout < interval.
- [x] Metrics: `iam_audit_log_catalog_plans_poll_total{result=success|error|timeout}`, `iam_audit_log_catalog_plans_stale_seconds` (a GaugeFunc measuring from process start until the first success), and `platform_dependency_request_seconds{target_service="iam-catalog-admin",endpoint="plans"}`. `check-metric-naming.sh` now recognises GaugeFunc.
- [x] AL-7 `GET /api/v1/internal/audit/events?tenant_id=…`: mesh-only (`RequireSystemRole` → `403 forbidden_peer`). `tenant_id` is required (UUID, else `400`) and bound to `app.tenant_id`, so RLS applies. No plan clamp (§5.4). It uses the same filter, keyset and hot+archived merge as AL-1, with `window_clamped` always false. An archived range over the bounds → `422 range_too_large` (D-14).
- [x] Tests:
  - unit: the poller (swap only on a `record_versions` change, the stale-if-error table, timeout vs error, the per-poll deadline, `Run` cadence, D-13 precedence through the query service), the client against httptest (path/headers, the real Catalog shape, the 1 MiB cap, timeout classification), the metrics adapter (counter plus the staleness gauge), config validation, AL-7 service and handler (no clamp, `422`, `403 forbidden_peer` for tenant roles, query-string `tenant_id`).
  - e2e: `TestAL7_ProvenanceReadNoClamp`, `TestAL7_TenantIsolationAndAuth`, `TestAL7_ArchivedRangeTooLarge_D14`, and `TestCatalogPoller_PlanWindowFromCatalog_ALD15`. The last runs a fake Catalog through a live window change, an outage served stale, and a version bump.
  - Merged coverage is 96.5%.
- Deferred by design: the RB-8 runbook and the stale-seconds alert thresholds (2×/10× interval) go to Phase 8.

### Phase 6 — Redaction (§4.2, §8.7, §15.5; AL-INV-7, AL-INV-12, AL-D10, AL-D13; decisions D-1, D-15..D-17) — ✅ done, awaiting review
- [x] Consuming `UserDeleted` writes the audit row and a `pending` task in one transaction (`AppendWithRedaction`). The trigger's envelope id makes a redelivery a no-op (`uq_redaction_trigger`, gap 35). `TenantOffboarded` raises no task (D-16).
- [x] Immediate redaction after commit through `apply_redaction(task_id)` (`000007`): SECURITY DEFINER, owned by `audit_reconciler`, EXECUTE granted to `audit_app` (D-1). It rewrites hot `security_3y` rows where the subject is the actor or user target (D-15), then decides `applied` / `not_applicable` / `missed` (D-17). It is idempotent: a finished task only reports its outcome. A failed apply never fails the message; the task stays `pending`.
- [x] Stuck-pending retry: the `cmd/reconciler --job=redaction-retry` CronJob (every 15 m, `REDACTION_RETRY_MIN_AGE`, `REDACTION_RETRY_BATCH`). Metric: `iam_audit_log_redaction_tasks_total{status}`. `missed` is routine (AL-Q15 Option A); `pending` means an apply failed and is what needs action.
- [x] Late arrivals (D-18): the ingest-time check against `redacted_subjects` inside `Append`, and the daily `redaction-sweep` job (`REDACTION_SWEEP_WINDOW`).
- [x] Tests:
  - unit: the `RedactionTrigger` table, the bus redaction path (request shape, redelivery not re-applied, a failed apply → `pending` and the message still acked, no-user_id fallback, `TenantOffboarded` never a trigger), the `redaction-retry` job, metrics, config, and the Helm CronJob contract.
  - postgres: `TestRedaction_ImmediateOnUserDeleted_ALINV12`, `TestRedaction_Compliance7yUntouched_ALINV7`, redelivery no-op, `not_applicable`, `TestRedaction_MissedWhenSubjectArchived_D17`, `TestRedaction_GrantsAndOwnership_D1` (definer owner, `audit_app` still INSERT/SELECT only, reconciler UPDATE limited to the two columns), and `PendingTasks`.
  - integration (floci SQS, the real fleet): `TestRedaction_BusUserDeletedEndToEnd`. It found the conflict-target 42501 bug (gap 35) before merge.
  - Merged coverage is 96.5%.
- **Phase 7 must:** fill `audit_archive_objects.subject_ids` (D-17) and refuse to archive a partition with a pending task (AL-INV-12 backstop).

### Phase 7 — Reconciler (§8.5, §8.6, §15.4, §13.1; AL-INV-9, AL-INV-12; decisions D-19, D-20) — ✅ done, awaiting review
- [x] `cmd/reconciler --job=` registry: `reconcile` (partitions → re-open → archive → verify → drop), `processed-events-prune` (8 d, batched; CronJob enabled), `redaction-retry` and `redaction-sweep` (Phase 6).
- [x] AL-INV-12 backstop: a partition touched by a pending redaction task is skipped (checked before archival and again inside the drop gate); `iam_audit_log_redaction_blocked_archive_total`.
- [x] S3 writer: parts keyed per tier and tenant (D-10), `jsonl.gz`, up to `ARCHIVE_PART_MAX_ROWS` each, SSE-KMS. Object Lock is set in `AUDIT_ARCHIVE_OBJECT_LOCK_MODE` with retain-until = max_occurred_at + the tier's compiled duration. The manifest records the body SHA-256, `subject_ids` (D-17), id and time ranges, and a per-tier manifest SHA-256. Verify re-reads every object, compares checksums, and checks the lock echo (in AWS). `access_90d` is recorded and becomes `expired` at drop.
- [x] DETACH + DROP only through `audit_drop_partition()`: both retained tiers `verified`, live counts = the unsealed manifest, no pending redaction, all under an ACCESS EXCLUSIVE lock. Otherwise the partition is kept and counted in `iam_audit_log_archive_stalled`.
- [x] Late-arrival re-open, including dropped months (D-19); re-archive semantics (D-20).
- [x] Tests:
  - unit: the archive service state machine (part splits per tenant and size, keys, checksums, `subject_ids`, re-open part offsets, retry-once on `count_mismatch`, 13 failure paths each keeping the partition, temp-file cleanup), lifecycle helpers, S3 lock/SSE/verify, metrics, config, the jobs, and `buildArchiveStore`.
  - postgres: every repository method; `TestReconciler_DropBlockedWhenUnverified_ALINV9`, `TestReconciler_DropCountMismatch_ALD4`, `TestReconciler_RefusesPendingRedaction_ALINV12`, `TestReconciler_DropSealsAndDrops_ALINV9`, `TestReconciler_ReopenDroppedMonth_D19` (DEFAULT swap keeps bad-clock rows, triggers and indexes), redaction invalidating a verified archive, and function grants.
  - integration (floci S3 with COMPLIANCE lock verified): happy path → AL-1/AL-2 served from S3; blocked by pending redaction; S3 failure → no drop; a late arrival re-archived then dropped; a dropped month re-opened and re-dropped with no duplicates.
  - Found before merge: partitions unreadable by the reconciler (gap 39), plus a Phase 3 wire test whose assertion only passed because of leftover shared-queue messages (now isolated and corrected).
  - Merged coverage is 96.3%.
- **Carried to Phase 8:** a metrics path for the reconciler CronJob (gap 40), `archive_lag_seconds` and `default_partition_rows_total` (gap 41).

### Phase 8 — Observability & hardening (§11, §20, §24)
- [ ] Full Tier-1/2/3 metric set; registry + naming test; `deploy/monitoring/app-alerts.yml` (DLQ > 0 Critical, default-partition > 0 Critical, stale thresholds 2×/10×, etc.).
- [ ] `docs/runbook.md` RB-1..RB-8.
- [ ] Log hygiene: IDs only at info, never `metadata`.

---

## B. Invariant → enforcement → test map

| ID | Enforcement | Test (named) |
|---|---|---|
| AL-INV-1 | `000003_roles` grants; `000004` trigger; `check-grants.sh` | `TestRLS_Case4_AppRoleCannotUpdateOrDelete_ALINV1` |
| AL-INV-2 | single `IngestService` / `AuditWriter.Append` for bus + direct-write | `TestDirectWrite_CreatedThenReplay_ALINV4` (Append/consumer); bus half asserted in Phase 3 |
| AL-INV-3 | FORCE RLS + fail-closed `rls_check_tenant`; app pool forced onto the transaction-local GUC path | `TestRLS_Case1/2/3_*_ALINV3` (Phase 1); `TestGUC_TransactionLocalNoLeakAcrossPooledBackend_ALINV3`, `TestGUC_ClearedAtCommit_ALINV3`, e2e `TestTenantHeaderReachesPostgresAsGUC_ALINV3` (Phase 0) |
| AL-INV-4 | `processed_events` + `uq_audit_events_source_id` | `TestIngest_CreateThenReplay_ALINV4`, `TestIngest_ReplayAfterLedgerPruneHitsBackstop_ALINV4`, `TestIngest_ConcurrentSameKeyYieldsOneRow_ALINV4`, e2e `TestAL5_CreateThenReplaySameID_ALINV4` |
| AL-INV-5 | NOT NULL provenance cols | `TestIngest_ProvenancePreserved_ALINV5` |
| AL-INV-6 | tier from taxonomy; compiled durations | `TestBuildDirectWriteEntry_DerivesTierAndShape_ALINV6_ALINV11`, `TestTaxonomy_EveryLLDBusRowMaps_ALD3` |
| AL-INV-7 | `apply_redaction()` `WHERE retention_tier='security_3y'` | `TestRedaction_Compliance7yUntouched_ALINV7`, integration `TestRedaction_BusUserDeletedEndToEnd` |
| AL-INV-8 | service-layer clamp, independent of tier; export filter stored clamped | `TestQuery_WindowClampIndependentOfTier_ALINV8`, e2e `TestAL1AL2_PlanWindowClamp_ALINV8` |
| AL-INV-9 | `audit_drop_partition()`: all retained tiers verified + live counts = unsealed manifest, under an exclusive lock | `TestReconciler_DropBlockedWhenUnverified_ALINV9`, `TestReconciler_DropSealsAndDrops_ALINV9`, integration `TestReconciler_S3FailureNoDrop_ALINV9` |
| AL-INV-10 | no publisher; CI grep; asyncapi receive-only | `check-forbidden-events-bypass.sh`, `check-outbox-access.sh`, `TestAsyncAPI_NoSendOperations_ALINV10`, `TestMigrations_CreateNoOutboxTables_ALINV10` |
| AL-INV-11 | caller tier ignored (unknown field) | `TestCreateEntry_StatusAndMapping_ALINV11`, e2e `TestAL5_CallerTierIgnored_ALINV11` |
| AL-INV-12 | redact-on-consume (Phase 6) + archival refusal (Phase 7) | `TestRedaction_ImmediateOnUserDeleted_ALINV12`, `TestReconciler_RefusesPendingRedaction_ALINV12`, integration `TestReconciler_BlockedByPendingRedaction_ALINV12` |

---

## C. Spec gaps / contradictions found (for LLD owner)

### Decisions taken (2026-09-25, user)
- **D-1 (gap 1):** Immediate redaction runs via a narrow `SECURITY DEFINER` function
  `apply_redaction(task_id uuid)` **owned by `audit_reconciler`**; `audit_app` gets `EXECUTE` on it and
  `INSERT` on `audit_redaction_tasks`, never `UPDATE` on `audit_events`. `cmd/server` calls it right after
  the `UserDeleted` ingest commits. The trigger's `current_user` check passes because definer context
  runs as the owner. **Needs an LLD note to §8.7/§15.5/§4.6.**
- **D-2 (gap 2):** The export worker is a goroutine in `cmd/server` running as `audit_app`. It claims jobs through a
  `SECURITY DEFINER claim_export_job()` that returns `(id, tenant_id)`, then streams rows under that tenant's
  `SET LOCAL` GUC, so RLS still applies. **Needs an LLD note to §4.2 (`audit_export_jobs`).**
- **D-4 (gap 4, 2026-09-26):** For AL-6, the `Idempotency-Key` header is still required (the batch request id), **and** every entry carries its own `idempotency_key` body field. Per-entry keys stay stable when a retry reorders entries, and a missing or invalid key fails only that index. The response is always HTTP **207** `{"results":[{"index","status","id"|"code","message"}]}`.
- **D-5 (§10.5, 2026-09-26):** AL-6 is rate-limited with an in-process per-tenant token bucket (`golang.org/x/time/rate`, as RP uses), keyed on `x-tenant-id` and answering `429 rate_limited`. New env vars: `INGEST_BATCH_RATE_LIMIT_RPS` (default 10) and `INGEST_BATCH_RATE_LIMIT_BURST` (default 20). **Needs an LLD §12 addition.**
- **D-6 (§5.4, 2026-09-26):** AL-5/AL-6 accept **only the direct-write rows** of §7.1: `config.*`, `tenant.owner_signed_up`, `security.cross_tenant_access`, `invitation.*`. Bus-sourced types must arrive on their owning topic; anything else gets `422 unknown_entry_type`. This is stricter than the LLD's literal wording. **Needs an LLD note to §5.4.**
- **D-7 (§7.1, 2026-09-26):** Workflow Engine's real wire types (`workflow.task.created`, `workflow.task.sla-warning`, `workflow.task.sla-breached`, `workflow.task.deferred`, `workflow.task.reassigned`, `workflow.instance.finished`, and `workflow.template.published` on `wf.template.events`) map to the same entry_types as the LLD's `TaskCreated`/… names. Both spellings are accepted. Other workflow wire types fall through to `workflow.unknown`. **Needs an LLD §7.1 correction.**
- **D-8 (§10.3, 2026-09-26): actor/target derivation for bus events.**
  1. `iam.auth.events`: the actor is a `user` taken from `data.user_id`/`subject` (the envelope actor is always the `…00a1` sentinel). No user → `anonymous`, with `attempted_username` staying in metadata (AL-D11).
  2. An actor of `"iam-system"`, `…00a1`, the nil UUID, or `ip_address="system"` → `iam_system`, with ip NULL and user_agent kept.
  3. Any other UUID actor → `user`.
  4. No actor, or an unrecognised label → `iam_system` with `metadata._actor_unattributed=true` (and `_actor_raw`), so the gap stays visible.

  The target is derived per topic from the payload (`user_id`, `tenant_id`, `delegation_id`, `principal_id`, `tender_id`, workflow ids), and metadata is the payload.
- **D-9 (§4.2 / AL-EVT-4, 2026-09-26):** a payload over `MAX_METADATA_BYTES` is persisted with `metadata = {"_truncated":true,"_original_bytes":N,"_sha256":"…"}` and the row is otherwise intact. It is never dropped or DLQ'd.
- **D-10 (gap 8, 2026-09-26, user):** Archive objects are partitioned by **tenant_id and time period**. The key is `{retention_tier}/{tenant_id}/{yyyy}/{mm}/audit_events_{yyyy}_{mm}-part-NNNN.jsonl.gz`; the per-tier prefix is kept so Object Lock and lifecycle rules stay per tier. An interactive archived read is served synchronously when the estimated result is **≤ 10,000 records and ≤ 50 MB** (`ARCHIVE_SYNC_MAX_ROWS`, `ARCHIVE_SYNC_MAX_BYTES`). Past either threshold it is converted into an async export (AL-3 job auto-created) and returns **202**. The estimate is the summed row/byte counts of the tenant's manifest objects in range: a conservative upper bound, since content filters can't be pre-applied. **Changes the §15.4/§25 key scheme — needs an LLD note.**
- **D-11 (§5.4, 2026-09-26):** A literal 7-day presigned URL is impossible under IRSA session credentials. The export object stays retrievable for 7 days (`EXPORT_SIGNED_URL_TTL`, stored as `signed_url_expires_at`), and every AL-4 poll of a ready job presigns a **fresh short-lived URL** (`EXPORT_DOWNLOAD_URL_TTL`, default 15 m). **Needs an LLD §5.4 wording correction.**
- **D-12 (§5.4, 2026-09-26):** The hot/archived boundary is **partition existence**, not a fixed 90-day cut. A month whose partition still exists is read from RDS, and only a month whose partition was dropped (after AL-INV-9) is read from the tenant's archive objects, so each row is read from exactly one place.
- **D-13 (§4.2/AL-D15 vs §5.4, 2026-09-26, user):** The LLD contradicts itself on the plan-window fallback. **§5.4 is kept:** the live CAT-I2 map for the row's `plan_code`, then the row's stored `query_window_days`, and `AUDIT_DEFAULT_QUERY_WINDOW_DAYS` only when there is no row. §4.2/AL-D15 would drop to the 365-day default on a cold start or an unrecognised `plan_code`, briefly shrinking an Enterprise tenant's window, which is exactly what AL-D15's rationale rejects. **Needs an LLD §4.2/§22 wording correction.**
- **D-14 (AL-7, 2026-09-26, user):** AL-7 applies the D-10 manifest estimate and returns **`422 range_too_large`** (a new §17 code) when the archived part of the range exceeds `ARCHIVE_SYNC_MAX_ROWS`/`_BYTES`. The caller narrows `from`/`to`; a service has no export path, since AL-4 is a tenant-admin route. Hot-only ranges are unaffected. **Needs an LLD §5.4/§17 addition.**
- **D-15 (gap 5, 2026-09-26, user): what redaction rewrites.** On `security_3y` rows only (AL-INV-7), `metadata` is replaced whole with `{"_redacted":true,"_redaction_task_id":…,"_redacted_at":…}` and `actor_display` is set to NULL where `actor_id` = the subject. Rows are selected where the subject is the **actor or the user target** (`target_type='user' AND target_id=subject`, using `idx_audit_events_tenant_target`), which is broader than the LLD's actor-only wording. `actor_id`/`target_id` are never touched. A whole-object marker needs no field-level PII list (AL-Q11); Legal's list, when published, can only narrow it. **Needs an LLD §8.7/§15.5 note.**
- **D-16 (gap 6, 2026-09-26, user):** `TenantOffboarded` raises **no** redaction task. It is audited as `tenant.offboarded`, and per-user redaction arrives through User Profile's `UserDeleted` fan-out (AL-Q14). The `'TenantOffboarded'` trigger value stays unused. **Needs an LLD §4.2 note.**
- **D-17 (gap 7, 2026-09-26, user): `missed` detection by manifest actor set.** Found while resolving gap 7: the LLD's premise that "every row it could touch is still within the 90-day hot window" only holds for the subject's *latest* rows. Anyone active for longer than the hot window already has archived `security_3y` rows that redaction can't reach. → `000007` adds `audit_archive_objects.subject_ids uuid[]` (GIN): the distinct actor ids and user-target ids per object, **written by the Phase 7 archiver**. `apply_redaction()` redacts the hot rows, then marks the task `missed` if any dropped-partition `security_3y` object of the tenant lists the subject. Otherwise it marks `applied` (rows > 0) or `not_applicable`. `rows_redacted` is always the hot count. `missed` is the **expected, routine** outcome for long-lived users. It is counted, logged at info and not alarmed; only a stuck `pending` needs action (RB-7). **AL-Q15 has been restated in LLD rev 0.22 as Option A** (archived rows retained; redaction guaranteed only in hot storage), and Legal is to confirm A or mandate B.
- **D-18 (gap 37, 2026-09-26, user): late arrivals after redaction — the ingest-time check is mandatory, backed by a daily sweep.** Every finished `apply_redaction()` of any status upserts `redacted_subjects(tenant_id, subject_id, task_id, redaction_completed_at)`, which is RLS-scoped; `audit_app` has SELECT. The single `Append` (AL-INV-2: bus and direct-write) checks it inside the insert transaction. A `security_3y` row whose actor or user target is an erased subject is redacted **before insert**: marker plus `_redacted_on_ingest`, and actor_display cleared. The **`redaction-sweep`** CronJob runs daily, calling `sweep_redactions(REDACTION_SWEEP_WINDOW = 90 d)` as audit_reconciler. It re-redacts any row that slipped past the check (a race with a concurrent apply, a bug, a backfill) and logs a non-zero count at warn. The marker has one SQL definition, `redaction_marker()`, and the Go `domain.RedactedMetadata` builds the same keys. LLD rev 0.22.
- **D-19 (AL-D4, 2026-09-26, user): late arrivals are re-opened automatically, including dropped months.**
  - *Not yet dropped:* `audit_drop_partition()` compares live per-tier counts with the unsealed manifest under an exclusive lock. A mismatch → `count_mismatch` → the retained tiers reset to pending and are re-archived in the same run, once. If writes keep arriving, the drop is deferred to the next run.
  - *Already dropped:* late rows land in `audit_events_default`. The reconciler finds DEFAULT rows whose month is `dropped`, and `audit_reopen_partition()` (a SECURITY DEFINER owned by the migrator) swaps DEFAULT out, recreates the month and re-routes the rows through the parent. No row is UPDATEd or DELETEd; the old DEFAULT is dropped whole. It then resets the tiers to pending.
  - DEFAULT rows for a month that was never dropped (a bad clock) stay with RB-3. **This goes beyond the LLD's manual RB-3; needs an LLD §4.2/RB-3 note.**
- **D-20 (2026-09-26, user): a re-archive rewrites the whole tier, and a manifest row is sealed at drop.** A re-archive re-uploads the tier's **unsealed** parts under the same deterministic keys, as new object versions. The bucket is versioned with Object Lock, so earlier versions stay locked until they expire; that is a storage cost only. Stale unsealed manifest rows are deleted. `audit_archive_objects.sealed` is set by the drop gate: a sealed object's rows exist only in S3, and it is never rewritten. A re-opened month appends new parts after its sealed ones (`NextParts`), because its partition now holds only the late rows. **D-12 is refined accordingly:** archived reads, AL-2, and redaction's `missed` check route on `sealed`, not on partition existence. Otherwise a re-opened month would hide its earlier, S3-only rows.
- **Stale-PII guard (Phase 7, resolution applied):** `apply_redaction()` and `sweep_redactions()` reset the `security_3y` archive state of every partition whose rows they rewrote (`invalidate_security_archive`). An object uploaded before a redaction is therefore re-archived and can never be sealed with pre-redaction PII. `MarkVerified` only promotes a tier that is still `archived`.
- **D-3 (gap 3):** Keep the library consumer. Add a Critical alert on `events_consumed_total{status="malformed"} > 0`
  and a runbook entry, and file a platform-events fix so malformed messages are left for the DLQ. This is a known gap against AL-EVT-4.

Blocking (need a decision before the phase noted; 1–3 resolved above):

1. **[Phase 1/6] Redaction role contradiction.** §8.7 has the `cmd/server` consumer insert `audit_redaction_tasks` and run `UPDATE audit_events SET metadata=…`, but `audit_app` has no `UPDATE` (AL-INV-1, §4.3). The trigger (§4.5) allows only `audit_reconciler`. §4.2/§4.6 say the tasks table is "reachable only by `audit_reconciler`". §15.5 and RB-7 say the redaction is "performed by `audit_reconciler`". Rule 5 of the implementation brief forbids `cmd/server` from holding that role.
2. **[Phase 1/4] Export worker placement.** §4.2 says "`cmd/reconciler` (or a dedicated worker)". But the reconciler is a daily CronJob (so exports could take up to 24 h), and `audit_export_jobs` is RLS-scoped, so a worker must claim jobs across tenants.
3. **[Phase 3] Malformed envelopes.** `platform-events` v1.4.0 *deletes* a message whose body isn't valid envelope JSON. It is neither retried nor DLQ'd, which contradicts AL-EVT-4 ("no silent drop") and §9. The library's other failure paths are fine.
4. **[Phase 2] AL-6 per-entry idempotency key.** The field name is unspecified ("each entry carries its own key, or the batch carries one and entries are indexed"). The response status is also unspecified (207 vs 200) and there is no body shape.
5. **[Phase 6] `redact(metadata)` semantics — resolved by D-15.** The field-level PII definition is AL-Q11's open ask, so what the MVP function actually rewrites is unspecified.
6. **[Phase 6] `TenantOffboarded` task — resolved by D-16.** The DDL allows `trigger_event_type='TenantOffboarded'`, but `subject_actor_id` is NOT NULL and a tenant event has no subject. User Profile already fans out a `UserDeleted` per user (AL-Q14).
7. **[Phase 6] `missed` detection — resolved by D-17.** Archived rows have no per-subject index, so the concrete rule for when the reconciler marks a task `missed` is unspecified.
8. **[Phase 4] Archived-read threshold — resolved by D-10.** "Bounded" vs "large" (the `202` → export case) is undefined. Archive objects are per tier/month *across all tenants*, so an archived interactive read means scanning other tenants' rows in app code, outside RLS.

Non-blocking (proposed resolution applied unless objected):

9. **`audit_export_jobs` `updated_at`.** §4.5 requires a `touch_row()` trigger, but the DDL has no `updated_at` column. → Add `updated_at timestamptz NOT NULL DEFAULT now()`.
10. **DSN env vars.** §12 lists only `DATABASE_URL`. → Add `RECONCILER_DATABASE_URL` and `MIGRATION_DATABASE_URL`, following the RP mechanics (RP uses `SYSTEM_DATABASE_URL`). `DB_APP_ROLE` / `DB_RECONCILER_ROLE` become role-name assertions checked at startup (`SELECT current_user`).
11. **Migration timing.** LLD §4.4 says run at startup; HLD §15.6 says a Helm pre-install Job. → Run at startup, which is what the LLD and RP both do. Flagged to the HLD owner.
12. **Mesh peer auth.** "mTLS peer identity" is in practice the gincommon header contract plus `RequireSystemRole`; no sibling reads XFCC. → Accept the `iam-system` role on `x-tenant-roles`; anything else gets `403 forbidden_peer`. Missing identity headers get gincommon's `401`.
13. **Migration table name.** §25 lists `schema_migrations`, but pgcommon's default is `pgcommon_migrations`. → Use the pgcommon default, as the siblings do.
14. **Library APIs that don't exist.** §3.3.3 cites `GlueCodec`, `ValidatingCodec`, `skipDuplicate` and `ackUnknown`; none are in platform-events v1.4.0. §3.3.1 cites `RequireAuth` / `ErrorResponse` usage. → Implement codecs locally in `outbound/glue` (mirroring Event Consumer). `skipDuplicate` is replaced by the `processed_events` ledger.
15. **No `platform-audit` library exists.** §18.2 describes it; RP and Catalog each hand-roll an AL-5 client. Informational only, and it confirms the wire contract: `Idempotency-Key`, `x-user-id: …00a1`, `x-tenant-roles: iam-system`, 201/200/422/429/5xx.
16. **Local AWS emulator.** Siblings use Floci, not LocalStack. → Use Floci for mechanics parity. **Verified in Phase 7 (floci 2.1.0-compat):** the Object-Lock bucket is versioned, and GetObject echoes lock mode and retain-until, so the integration tests run COMPLIANCE with read-back verification. **One divergence from AWS:** floci refuses a second PutObject to a key under COMPLIANCE retention (403), whereas S3 stores it as a new version. D-20's rewrite-as-new-version path (late-arrival re-archive, a failed-tier retry) is therefore exercised locally only without a lock. The AWS behavior is by S3 contract. `TestReconciler_FlociObjectLockSupport` pins both behaviors so a floci upgrade surfaces the change. **Accepted (2026-09-27, user): severity Low, LLD/test note only (LLD rev 0.23 §14). Not a release blocker; the production path is designed around documented S3 versioning behavior.**
17. **Redaction status name.** The brief says a task is marked `redacted`; the LLD enum says `applied`. → Use the LLD value `applied`.
18. **HLD §13.3 vs LLD §13 on archive replication.** §13.3 says "cross-region replication not in scope at MVP", while §13.2 and the LLD say the archive is cross-region replicated. This is infrastructure, not code. Flagged only.

Found during Phase 0 (non-blocking; resolution applied, flag for LLD):

19. **Error body shape.** §17 says `{code, message, request_id}`, but `gincommon.ErrorResponse` is `{error, status, trace_id, request_id}`. → Emit a superset (`error` = `code`, plus `message`, `status`, `trace_id`, `request_id`, `details?`), matching iam-catalog-admin's envelope.
20. **AL-6 path `…/audit-entries:batch` — resolved (Phase 2).** gin v1.12 routes an escaped literal colon (`audit-entries\:batch`) alongside `audit-entries`. The wire path is exactly as the LLD states.
21. **`core/port` vendor use.** The logger port's trace-id helper imports `otel/trace`. That is the only vendor the port may use, declared explicitly in `.go-arch-lint.yml`. §3.2 says "domain only", so this is a narrow, recorded exception.
22. **`core/domain` stays stdlib-only**, as §3.2 requires. That is stricter than RP, whose domain imports `uuid`. Sentinels are canonical strings, and UUIDv7 minting in Phase 2 will be stdlib-only or sit behind a port.
23. **401 code name.** §5.5 lists 401 without a code. → Use the sibling code `missing_identity_headers`.
24. **Migration ordering.** §4.4 lists four migrations and places neither `audit_redaction_tasks` (added rev 0.3) nor the §4.5 triggers. → `000001` includes `audit_redaction_tasks`; triggers get their own `000004_triggers`; partition bootstrap becomes `000005`.
25. **Local ports.** PG `5548`, floci `4573` (sibling ports `5544` / `4566–4572` are taken).

Found during Phase 1 (resolution applied; flag for LLD):

26. **Partition DDL for the runtime roles.** §4.4 has the server pre-create partitions at startup, but `audit_app` has no DDL rights. In any case, CREATE/DETACH/DROP PARTITION require ownership of the parent, which no grant can confer. → `audit_ensure_partitions()` is a SECURITY DEFINER function owned by the migrator, bounded to 0..24 months each side, with EXECUTE granted to `audit_app` and `audit_reconciler`. This is the same narrow-definer pattern as D-1/D-2. Phase 7 adds a drop function in the same style that enforces AL-INV-9 in the database. **Needs an LLD note to §4.3's role table.**
27. **[Phase 2] Constraint names on partitions.** A unique violation names the partition's index (`audit_events_YYYY_MM_source_event_id_occurred_at_idx`), not `uq_audit_events_source_id`. Error mapping must key on SQLSTATE 23505 plus that suffix.
28. **[Phase 2] jsonb under PgBouncer mode.** The app pool always uses PgBouncer mode (AL-INV-3 fix), and in PgBouncer mode pgx encodes a `[]byte` argument as bytea hex, which a `jsonb` column rejects. `metadata` and `filter` must be bound as `string` or `json.RawMessage`→`string`, never raw `[]byte`. RP hit the same issue on `outbox_events`.

Found during Phase 4 (resolution applied; flag for LLD):

29. **Per-object archive manifest — documented in LLD rev 0.21 (2026-09-26, user).** §4.2's `audit_event_archive_state` is per partition × tier. It can't give the D-10 size estimate, locate one tenant's objects, or find an archived id. → `000006` adds `audit_archive_objects`, one row per S3 object: tier, tenant, month, part, row/byte counts, `occurred_at` and **id** ranges, and sha256. It is RLS-scoped like `audit_events`; `audit_app` gets SELECT and `audit_reconciler` gets SELECT and INSERT. The LLD now covers the table (§4.2), the ER diagram, §4.3/§4.6, the tenant-partitioned key scheme (§15.4) and §25. **The Phase 7 archiver must write a row per object, including `min_id`/`max_id`.**
30. **Export objects share the archive bucket — accepted as an infra/operational note (2026-09-26, user).** Export objects live at `exports/{tenant}/{export_id}.jsonl.gz` (§25) in `AUDIT_ARCHIVE_BUCKET`. If that bucket sets a *default* Object Lock retention, every export would be locked for years. → Infra must use per-object retention for archive objects only (the Phase 7 writer sets it) and add a lifecycle expiry on `exports/` of ≥ `EXPORT_SIGNED_URL_TTL`. Flagged to infra; no code impact.
31. **Export dedup — removed (2026-09-26, user).** Every AL-3 call, and every D-10 deferral from AL-1, creates a new job. Abuse control is the per-tenant AL-3 limiter (§10.5, `EXPORT_RATE_LIMIT_*`). `ExportJobs.FindActive` is gone.
32. **AL-2 on archived entries — fixed (2026-09-26, user).** AL-2 reads the hot store first. On a miss it asks the manifest for the dropped-partition objects whose `[min_id, max_id]` contains the id and that reach the window start, reads only those, and stops at the first match. The plan-window check still applies to the result. With D-12, a month lives in exactly one place, so the fallback can never return a stale duplicate. A missing object is skipped with a warning; any other S3 error → `503`. Tests: `TestQuery_GetFallsBackToArchive`, `TestQuery_ArchivedObjectsForID_Gap32`, e2e `TestAL2_ArchivedEntryReadable_Gap32`.
33. **New §12 env vars.** `ARCHIVE_SYNC_MAX_ROWS` (10000) and `ARCHIVE_SYNC_MAX_BYTES` (50 MiB) come from D-10, and `EXPORT_DOWNLOAD_URL_TTL` (15m) from D-11. `EXPORT_RATE_LIMIT_PER_MINUTE` (10) and `EXPORT_RATE_LIMIT_BURST` (5) set the AL-3 bucket (§10.5). `EXPORT_POLL_INTERVAL` (5s), `EXPORT_JOB_LEASE` (15m, 1m..24h, heartbeated at lease/3) and `EXPORT_WORK_DIR` (default the `/tmp` emptyDir) cover the worker (D-2). `AUDIT_HOT_WINDOW_DAYS` no longer routes queries (D-12); only the reconciler uses it.

Found during Phase 6 (resolution applied; flag for LLD / infra):

34. **Owner of `apply_redaction()` (D-1) — deployment prerequisite, in `RELEASE_CHECKLIST.md` (2026-09-26, user); not an LLD issue.** `ALTER FUNCTION … OWNER TO audit_reconciler` requires the migrating role to be able to `SET ROLE audit_reconciler`. That holds for the superuser in dev/test; in AWS, **Terraform must grant `audit_reconciler` membership to `audit_migrator`**, or migration `000007` fails loudly. `audit_reconciler` gets a **column-level** `UPDATE (metadata, actor_display)` on `audit_events`, and the 000004 trigger still rejects every other role (AL-INV-1).
35. **Task insert without SELECT.** `audit_app` holds INSERT only on `audit_redaction_tasks` (D-1), so the task id is minted in the service (UUIDv7) and inserted with a target-less `ON CONFLICT DO NOTHING` and no `RETURNING`. A conflict target or `RETURNING` needs SELECT and fails with 42501; the integration test caught this. `RowsAffected` tells a new task from a redelivered trigger. A redelivered `UserDeleted` whose first apply failed does not re-apply inline; the **`redaction-retry` CronJob** (every 15 m, tasks pending ≥ `REDACTION_RETRY_MIN_AGE`) does, as audit_reconciler.
36. **A `UserDeleted` with no usable `user_id`.** No task can be created (`subject_actor_id` is NOT NULL). The audit row is still stored, and the event is logged at **error** so the unscheduled erasure is visible. No producer is known to send this.
37. **Late-arriving rows after redaction — resolved by D-18 (2026-09-26, user).** An ingest-time check (mandatory) plus the daily `redaction-sweep` (defense in depth).
38. **"Audited as a `security.cross_tenant_access`-class action" (§15.5).** Redaction writes no extra audit entry. The triggering `user.deleted` row plus the `audit_redaction_tasks` ledger (task id stamped into each redacted row's marker) are the trail. Flagged for the LLD.

Found during Phase 7 (resolution applied; flag for LLD):

39. **The reconciler cannot read partitions directly.** Grants on the parent `audit_events` do not extend to naming a partition table. The archive repository therefore reads through the parent, bounded to the month (`occurred_at >= m AND < m+1`), which PostgreSQL prunes to that one partition. The postgres tier caught this before merge. The definer functions (`audit_drop_partition`, `audit_reopen_partition`) are migrator-owned and address partitions directly.
40. **Reconciler metrics have no scrape endpoint — accepted as a known observability limitation until Phase 8 (2026-09-27, user; severity Medium; LLD rev 0.23 §11; not a design flaw and not blocking approval).** `archive_stalled`, `archive_partitions_total`, `redaction_blocked_archive_total`, `retention_pruned_total` and `redaction_tasks_total` (from the retry job) are incremented in a CronJob that exposes no `/metrics`. A Critical alert cannot fire from them until **Phase 8** adds a push path, or has cmd/server export gauges derived from `audit_event_archive_state`. Meanwhile, a blocked or stalled run exits non-zero, so the CronJob failure is the signal.
41. **`archive_lag_seconds` and `default_partition_rows_total`** (§11) are deferred to Phase 8 for the same reason.
