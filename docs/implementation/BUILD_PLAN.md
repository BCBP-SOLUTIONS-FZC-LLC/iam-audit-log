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

### Phase 4 — Query + export (§5.1–§5.4 AL-1..AL-4, §8.4, §15.3; AL-INV-8, AL-D5)
- [ ] `RequireAuditReader` (`tenant_admin` ∨ `tenant_owner`) → `403 insufficient_permissions`.
- [ ] Window resolution precedence (§5.4): poller map[plan_code] → row.query_window_days → `AUDIT_DEFAULT_QUERY_WINDOW_DAYS` (no row); clamp + `window_clamped`/`effective_from`.
- [ ] Keyset on `(occurred_at DESC, id DESC)`, opaque cursor; AL-2 single read.
- [ ] Hot/archived routing at `AUDIT_HOT_WINDOW_DAYS`; archived reader; `202` for large ranges.
- [ ] AL-3/AL-4 export jobs; S3 SSE-KMS object; 7-day presigned URL.
- [ ] No cache (AL-D5).

### Phase 5 — CAT-I2 poller + AL-7 (§4.2, §12, AL-D15; §5.2/§5.3 AL-7)
- [ ] `outbound/catalog` client: `GET {CATALOG_BASE_URL}/api/v1/internal/plans`, headers `x-user-id: iam-system`, `x-tenant-id: 00000000-…0000`, `x-tenant-roles: iam-system` (matches Catalog `RequireSystemRole` + O&M client), `CATALOG_PLANS_POLL_TIMEOUT`, 1 MiB body cap.
- [ ] Response: `{plans:[{code, audit_query_window_days, record_version,…}], record_versions:{code:int64}}`; swap map only when `record_versions` differs.
- [ ] Stale-if-error; `catalog_plans_poll_total{result}`, `catalog_plans_stale_seconds`.
- [ ] AL-7 `GET /api/v1/internal/audit/events?tenant_id=…` mesh-only, RLS-scoped, no plan clamp (§5.4 "retrievable … by compliance/legal via AL-7").

### Phase 6 — Redaction (§4.2, §8.7, §15.5; AL-INV-7, AL-INV-12, AL-D10, AL-D13)
- [ ] `UserDeleted` consume: audit row + `audit_redaction_tasks` pending in one tx (unique trigger id = no-op on redelivery).
- [ ] Immediate redaction of hot `security_3y` rows via `idx_audit_events_tenant_actor`; `applied` / `not_applicable`.
- [ ] Stuck-pending retry; `missed` detection; `redaction_tasks_total{status}`.
- [ ] Tests: compliance_7y untouched; redelivery no-op.

### Phase 7 — Reconciler (§8.5, §8.6, §15.4, §13.1; AL-INV-9, AL-INV-12)
- [ ] `cmd/reconciler --job=` registry (RP): `reconcile` (partitions → archive → verify → drop), `processed-events-prune` (8 d).
- [ ] Redaction-pending archival refusal (AL-INV-12 backstop) + `redaction_blocked_archive_total`.
- [ ] S3 writer: per-tier prefix, `jsonl.gz` parts, SSE-KMS, Object Lock COMPLIANCE retain-until = tier; manifest SHA-256; verify; `access_90d` → `expired`.
- [ ] DETACH + DROP only when all retained tiers `verified`; else `archive_stalled`.
- [ ] Late-arrival re-open for a `verified` month (AL-D4).
- [ ] Tests (floci S3): happy path; blocked drop; S3 failure → no drop.

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
| AL-INV-7 | redaction `WHERE retention_tier='security_3y'` | `TestRedaction_Compliance7yUntouched_ALINV7` |
| AL-INV-8 | service-layer clamp, independent of tier | `TestQuery_WindowClampIndependentOfTier_ALINV8` |
| AL-INV-9 | drop gated on all-verified | `TestReconciler_DropBlockedWhenUnverified_ALINV9` |
| AL-INV-10 | no publisher; CI grep; asyncapi receive-only | `check-forbidden-events-bypass.sh`, `check-outbox-access.sh`, `TestAsyncAPI_NoSendOperations_ALINV10`, `TestMigrations_CreateNoOutboxTables_ALINV10` |
| AL-INV-11 | caller tier ignored (unknown field) | `TestCreateEntry_StatusAndMapping_ALINV11`, e2e `TestAL5_CallerTierIgnored_ALINV11` |
| AL-INV-12 | redact-on-consume + archival refusal | `TestRedaction_ImmediateOnUserDeleted_ALINV12`, `TestReconciler_RefusesPendingRedaction_ALINV12` |

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
- **D-3 (gap 3):** Keep the library consumer. Add a Critical alert on `events_consumed_total{status="malformed"} > 0`
  and a runbook entry, and file a platform-events fix so malformed messages are left for the DLQ. This is a known gap against AL-EVT-4.

Blocking (need a decision before the phase noted; 1–3 resolved above):

1. **[Phase 1/6] Redaction role contradiction.** §8.7 has the `cmd/server` consumer insert `audit_redaction_tasks` and run `UPDATE audit_events SET metadata=…`, but `audit_app` has no `UPDATE` (AL-INV-1, §4.3). The trigger (§4.5) allows only `audit_reconciler`. §4.2/§4.6 say the tasks table is "reachable only by `audit_reconciler`". §15.5 and RB-7 say the redaction is "performed by `audit_reconciler`". Rule 5 of the implementation brief forbids `cmd/server` from holding that role.
2. **[Phase 1/4] Export worker placement.** §4.2 says "`cmd/reconciler` (or a dedicated worker)". But the reconciler is a daily CronJob (so exports could take up to 24 h), and `audit_export_jobs` is RLS-scoped, so a worker must claim jobs across tenants.
3. **[Phase 3] Malformed envelopes.** `platform-events` v1.4.0 *deletes* a message whose body isn't valid envelope JSON. It is neither retried nor DLQ'd, which contradicts AL-EVT-4 ("no silent drop") and §9. The library's other failure paths are fine.
4. **[Phase 2] AL-6 per-entry idempotency key.** The field name is unspecified ("each entry carries its own key, or the batch carries one and entries are indexed"). The response status is also unspecified (207 vs 200) and there is no body shape.
5. **[Phase 6] `redact(metadata)` semantics.** The field-level PII definition is AL-Q11's open ask, so what the MVP function actually rewrites is unspecified.
6. **[Phase 6] `TenantOffboarded` task.** The DDL allows `trigger_event_type='TenantOffboarded'`, but `subject_actor_id` is NOT NULL and a tenant event has no subject. User Profile already fans out a `UserDeleted` per user (AL-Q14).
7. **[Phase 6] `missed` detection.** Archived rows have no per-subject index, so the concrete rule for when the reconciler marks a task `missed` is unspecified.
8. **[Phase 4] Archived-read threshold.** "Bounded" vs "large" (the `202` → export case) is undefined. Archive objects are per tier/month *across all tenants*, so an archived interactive read means scanning other tenants' rows in app code, outside RLS.

Non-blocking (proposed resolution applied unless objected):

9. **`audit_export_jobs` `updated_at`.** §4.5 requires a `touch_row()` trigger, but the DDL has no `updated_at` column. → Add `updated_at timestamptz NOT NULL DEFAULT now()`.
10. **DSN env vars.** §12 lists only `DATABASE_URL`. → Add `RECONCILER_DATABASE_URL` and `MIGRATION_DATABASE_URL`, following the RP mechanics (RP uses `SYSTEM_DATABASE_URL`). `DB_APP_ROLE` / `DB_RECONCILER_ROLE` become role-name assertions checked at startup (`SELECT current_user`).
11. **Migration timing.** LLD §4.4 says run at startup; HLD §15.6 says a Helm pre-install Job. → Run at startup, which is what the LLD and RP both do. Flagged to the HLD owner.
12. **Mesh peer auth.** "mTLS peer identity" is in practice the gincommon header contract plus `RequireSystemRole`; no sibling reads XFCC. → Accept the `iam-system` role on `x-tenant-roles`; anything else gets `403 forbidden_peer`. Missing identity headers get gincommon's `401`.
13. **Migration table name.** §25 lists `schema_migrations`, but pgcommon's default is `pgcommon_migrations`. → Use the pgcommon default, as the siblings do.
14. **Library APIs that don't exist.** §3.3.3 cites `GlueCodec`, `ValidatingCodec`, `skipDuplicate` and `ackUnknown`; none are in platform-events v1.4.0. §3.3.1 cites `RequireAuth` / `ErrorResponse` usage. → Implement codecs locally in `outbound/glue` (mirroring Event Consumer). `skipDuplicate` is replaced by the `processed_events` ledger.
15. **No `platform-audit` library exists.** §18.2 describes it; RP and Catalog each hand-roll an AL-5 client. Informational only, and it confirms the wire contract: `Idempotency-Key`, `x-user-id: …00a1`, `x-tenant-roles: iam-system`, 201/200/422/429/5xx.
16. **Local AWS emulator.** Siblings use Floci, not LocalStack. → Use Floci for mechanics parity. Object Lock support in Floci still needs verifying in Phase 7.
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
