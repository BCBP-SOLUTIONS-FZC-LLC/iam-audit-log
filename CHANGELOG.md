# Changelog

All notable changes to this service. Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versioning: see `VERSIONING.md`.

## [Unreleased]

### Added — Commercial domain support: CD-17 source constants + CD-21 entitlement queue (2026-10-15)
- **CD-17: `CommercialSource*` constants** in new `internal/core/domain/commercial_sources.go` — canonical envelope source names for `commercial-billing`, `commercial-entitlements` and `commercial-usage-metering` (Commercial HLD §9.3). Tests `TestBuildBusEntry_CommercialSources_CD17` and `TestIngestBus_CommercialSources_CD17` verify each agreed name is stored verbatim as `source_service` and triggers correct entry-type classification.
- **CD-21: 12th inbound queue `entitlement-audit-q`** for `entitlement.events` (Commercial Entitlements service):
  - `internal/config/config.go`: `ENTITLEMENT_AUDIT_QUEUE_URL` added as 12th queue.
  - `api/asyncapi.yaml`: `entitlementAudit` channel + `entitlementAuditQueue` server added; `TenantSeatsChanged` (billing), `TenantQuotaRestored` (usage) and `TenantEntitlementsChanged` (entitlement) messages added. Total receive operations 11 → **12**; total messages 65 → **69**.
  - `internal/core/domain/taxonomy.go`: 10 new entry types — `entitlement.tenant_changed` (security_3y), `tenant.seats_changed` (security_3y), `usage.quota.restored` (access_90d), `config.entitlement_override.{set,removed,expired}` (security_3y direct-write), `billing.adjustment.issued`, `billing.refund.issued` (security_3y direct-write), `commercial.cross_tenant_job.ran` (security_3y direct-write). Vocabulary 73 → **82**.
  - `deploy/helm/values.yaml`, `deploy/iam/policy.json`, `.env-example`: entitlement queue wired and the IRSA ARN added.
  - `test/unit/queue_catalog_test.go`: queue count 11 → 12; `taxonomy_test.go`: new bus and direct-write entries; vocabulary assertion 72 → 82.
- **Integration test `TestBus_CommercialEntitlementSource_CD17`** pins the `commercial-entitlements` source name on `entitlement-audit-q`.

### Fixed — production hardening (2026-10-15)
- **Archive crash ordering:** `archive_service.go` now writes the manifest row (`UpsertObject`) **before** the S3 `PutArchive` call. A crash between the two previously left an Object-Lock-held S3 version with no manifest row — permanently untrackable for 3–7 years. The reversed order leaves an orphaned unsealed manifest row, which `FinishArchive` removes on the next successful reconcile run.
- **S3 `PutArchive` retry:** up to 3 attempts with exponential backoff (1 s, 2 s) on `ErrDependencyUnavailable`; body rewound with `Seek(0)` on each retry; permanent errors returned immediately without retry.
- **Export max-rows cap:** `ExportService` enforces `DefaultExportMaxRows` (5 000 000). An unbounded export query could previously exhaust ephemeral storage; it now returns an error and marks the job failed.
- **Per-stage graceful shutdown:** `cmd/server/main.go` uses separate `context.WithTimeout` calls — HTTP 20 s, metrics 5 s, pool 30 s — instead of a single shared 30 s context. `terminationGracePeriodSeconds` bumped 45 → **80** in `deploy/helm/values.yaml`.
- **Plan poller startup jitter:** `PlanPoller.Run` waits a random `[0, Interval/2)` delay before the first poll, derived from `UnixNano`. Prevents all replicas from stampeding `iam-catalog-admin` (CAT-I2) simultaneously on a rolling restart.
- **Deterministic validation error field order:** `BuildDirectWriteEntry` iterates required-field checks as an ordered struct array instead of a `map[string]string`. When multiple fields are missing, the first error returned is now always `entry_type → action → source_service → source_event_type`, not random.

### Changed — dependency update (2026-10-15)
- **`github.com/klauspost/compress` v1.20.1 → v1.20.3** (patch; used transitively by AWS SDK v2 and platform-events SQS client).
- **govulncheck:** 0 vulnerabilities found across all production packages. One transitive module has a vulnerability in code this service does not call (reported as informational).

### Added — user.status.changed entry type (2026-10-13)
- **New `user.status.changed` (security_3y) entry type added to taxonomy** (compatibility fix UP-AL-GAP-004 — iam-user-profile bilateral check).
  - `internal/core/domain/taxonomy.go`: `user.status.changed` added to `entryTypes` and `{TopicUser,"UserStatusChanged"}` added to `busTypes`.
  - `internal/core/domain/taxonomy_test.go`: `UserStatusChanged` row added to `lldTable`; vocabulary count bumped 72 → 73.
  - `api/asyncapi.yaml`: `userAudit_UserStatusChanged` message added to `userAudit` channel and `components/messages`.
  - `docs/lld/iam-lld-audit-log-service.md`: §7.1 table row and §25 frozen vocabulary entry added.

### Changed — library upgrades (2026-10-13)
- **`platform-events/v2` bumped v2.0.0 → v2.1.0** (compatibility fix UP-AL-GAP-005 / DLG-AL-GAP-004 / EC-AL-GAP-001): aligns with all IAM producer services now on v2.1.0.
- **`platform-pgcommon/v2` bumped v2.0.0 → v2.0.1** (compatibility fix OM-AL-GAP-007 / RP-GAP-021): patch-level alignment with iam-org-membership, iam-realm-provisioner, and iam-delegation.

### Changed — asyncapi.yaml topic name documentation (2026-10-13)
- **`membership-audit-q` and `tenant-audit-q` channel titles and descriptions corrected** to use the actual SNS topic naming convention (`iam-membership-events`, `iam-tenant-events` — hyphen, owned by iam-org-membership and iam-realm-provisioner). Infrastructure verification note added. `TopicMembership`/`TopicTenant` taxonomy constants (internal lookup keys) unchanged. Tracked as OM-AL-GAP-005 (Infra Gap).
- **`auth-audit-q` channel title and description corrected** to use actual SNS topic name `iam-auth-events` (hyphen, owned by iam-event-consumer). Tracked as EC-AL-GAP-004 Infra note.

### Changed — e2e runs as its own CI job (2026-10-04)
- **`validate-test.yml` has two parallel jobs, `test` and `e2e`, as in iam-authz-enrichment.**
  - Before, e2e was a step after `make test-ci` inside the 40-minute `test` job.
  - Now the `e2e` job checks out, runs `make build`, then `make test-e2e` with `E2E_BINARY_DIR` pointing at `bin/`, so the process tests run the exact binaries the image ships.
  - A process or network failure shows up under its own job.
  - `needs: validate-test` in `ci.yml` and `release.yml` waits for both jobs, so e2e still gates trivy, smoke, push and release.
- **Fixed (CI):** the dashboard panel "RLS violations by type" queries `iam_rls_violations_total`, which is *proposed* since it is built from its registry entry, so it is now marked `(Proposed)` (`metrics-registry-lint.sh`).
  - `make invariant-lint` now also runs `metrics-registry-lint.sh` and `check-outbox-access.sh`, so `make ci` locally runs every `.github/scripts` gate CI runs.
- `test/e2e` `buildBinary` uses `E2E_BINARY_DIR` when it is set, and fails with a clear message if the binary is missing; locally it still compiles `./cmd/<name>`.

### Added — tests for previously uncovered behavior (2026-10-04)
- `TestReconcilerRepositories_RefusedAsAuditApp` (postgres): rule 5 / AL-INV-1.
  - Every archival, prune, redaction-retry and drop step run as `audit_app` is refused with 42501, and the error surfaces.
  - What `audit_app` may read stays fail-closed (no rows without a tenant GUC).
- `TestRun_JobLease` (cmd/reconciler): a run whose lease is held is skipped with exit 0 and never runs the job; a free run releases its lease; a database without `000011` fails the run instead of running unguarded.
- `TestServer_BootsWithConsumerFleetAndDrains` (cmd/server): with queues configured, the fleet is built (Glue endpoint override, DLQ forwarding), the server stays live while SQS is unreachable, and SIGTERM drains to exit 0.
- Unit tests:
  - A truncated zlib body is rejected by the Glue codec.
  - The bus `tenant.*` membership target, and the no-target delegation case.
  - The `source_topic` mapping on AL-1/AL-2 responses.
  - A nil `NextParts` map starts parts at 0.
  - `partWriter.discard` is idempotent.
- The floci fixture waits up to 240 s for `init-floci.sh` (it exceeded 120 s on a loaded Docker host).
- The Postgres fixture waits for the published 5432 port as well as the ready log line, and retries a container whose DSN cannot be resolved. On a loaded Docker host, `TestConstraint_AnonymousActor_ALD11` failed with `postgres dsn: port "5432/tcp" not found` before any SQL ran.

### Fixed — production-readiness review (2026-10-04)
- **Security: `/api/v1/internal/*` is identity-checked by the mesh.** Before, it was only a header check (`x-tenant-roles: iam-system`) behind a NetworkPolicy that admitted every pod in the namespace. Any such pod could forge AL-5/AL-6 entries for any tenant, or read any tenant through AL-7.
  - New `templates/mesh.yaml`: an Istio `AuthorizationPolicy` allows `/api/v1/internal/*` only from `mesh.authorizationPolicy.internalCallerPrincipals`, and `/api/v1/audit/*` only from the gateway. A `PeerAuthentication` is added too.
  - The NetworkPolicy admits only `networkPolicy.internalCallers`, and the Catalog egress only Catalog Admin's pods.
  - Server pods are labelled for sidecar injection; CronJob pods opt out.
  - `APP_ENV=prod` refuses to render without the policy unless `mesh.acknowledgeNoMesh=true`. An empty caller list fails too.
  - Checked by `TestHelm_InternalRoutesRequireMeshIdentity`.
- **Malformed and undecodable messages go to the DLQ** (decision D-22, supersedes D-3). `events.WithDLQForwarding` with platform-events' SQS DLQ publisher, wired once in `cmd/server`. IAM: `sqs:SendMessage`/`GetQueueUrl` on the audit DLQs only. The alert, RB-10 and runbooks are updated. `TestWire_MalformedEnvelopeForwardedToDLQ_D22` replaces the test that pinned the delete.
- **Database timeouts:**
  - Helm server: `PG_STATEMENT_TIMEOUT=30s`, `PG_LOCK_TIMEOUT=5s`.
  - Helm reconciler: `25m` / `10s`. The month scan fits under `RECONCILER_TIMEOUT`, and a partition lock gives up rather than queue ingest behind it.
- **Archived reads are bounded:** `ARCHIVE_READ_CONCURRENCY` (default 2, 1–32) synchronous archived reads per replica. Each can hold up to the sync bounds in memory against the 512Mi limit. The rest wait within their request deadline.
- **One reconciler run per job:** migration `000011_reconciler_lease`. A run claims a lease row (released at the end, or expiring after the run deadline), so a manually created Job no longer overlaps a scheduled one. Covered by `TestJobLease_OneRunPerJob`.
- **Background loops survive a panic:** the catalog poller, export worker and ops monitor run under `supervise`, which recovers, logs the stack and restarts the loop after 5 s.
- **Alert runbook links:** every alert has an absolute `runbook_url` whose anchor is a real heading. Four alerts had none, and the relative `docs/runbook.md#rb-N` anchors matched no GitHub heading. Checked by `TestAlerts_RunbookURLsResolve`.
- **Tests:**
  - `TEST_POSTGRES_TIMEOUT` (1500s). The postgres package needs about 6–7 min under `-race`, and the old 300 s limit timed it out on every run.
  - The child-process tests forward `-test.gocoverdir`, so their coverage counts again.
- **Release checklist:** the KMS key ARN (the alias placeholder grants nothing), the DLQ send permission, and the Istio prerequisites.

### Changed — platform-library environment aligned with iam-org-membership (2026-10-04)
- **Helm `env` and `.env-example` now pass every variable the libraries read on this service's paths:**
  - gincommon tracing: `OTEL_EXPORTER_OTLP_INSECURE` (`false`), `OTEL_TRACES_SAMPLER_RATIO` (`0.1`), `OTEL_PROPAGATOR_BAGGAGE` (`false`), `OTEL_TRACES_IGNORE_REMOTE_PARENT_SAMPLED` (`false`);
  - gincommon logger: `LOG_LEVEL` (`info`) and `LOG_SAMPLING` (`false`);
  - platform-events `config.LoadSQS`: `SQS_HANDLER_TIMEOUT` (`20s`, below the 30 s visibility timeout) and `SQS_DRAIN_TIMEOUT` (`15s`). Before, the drain used the library's 30 s default per queue.
- **Removed, never read:**
  - `OTEL_SERVICE_NAME` (Helm `serverEnv` and `reconcilerEnv`): both binaries pass the service name to `InitTracingWithConfig`, and the trace names are unchanged;
  - `APP_NAME` (`.env-example`, the Makefile export);
  - `PG_SSLMODE` (`.env-example`, tests): every pool gets an explicit DSN, which carries sslmode.
- **`Fleet.Stop` stops the 11 consumers concurrently.** Sequentially, the later queues kept receiving while the earlier ones drained, and a shutdown could take up to 11 drain timeouts against the 45 s grace period. Covered by `TestFleet_StopsConsumersConcurrently`.
- **Reconciler startup:** `ReconcilerPoolConfig` drops pgcommon's "PG_USER/PG_DBNAME required … connections will fail" warning. `cmd/reconciler` has no `DATABASE_URL` (rule 5), so the warning fired on every run although the pool uses `RECONCILER_DATABASE_URL`. Pool-tuning warnings are kept.

### Added — docs/observability and metricslint in CI (2026-10-04)
- **`docs/observability/`**, mirroring iam-org-membership:
  - `README.md`: the standard as implemented here (identity, tiers, who counts what, proposed metrics in live rules, adding a metric, CI checks, SLOs, HPA).
  - `metric-registry.md`: the inventory of every emitted metric, **generated** from the pinned gincommon registry and a real scrape.
  - `migration.md`: renamed and removed names across gap 46, the library migration and gincommon v1.6.0.
  - `runbooks.md`: per-alert triage for all 30 alerts, linking the full RB procedures in `docs/runbook.md`.
- **`make metrics-lint` / `make metrics-inventory`** (`.github/scripts/metricslint.sh`, `metrics-inventory.py`), in `make ci` and Validate / Quality. They run platform-gincommon's `metricslint check` on the full scrape, `metricslint refs` on `deploy/` and the operator Markdown, and the inventory drift check.
  - `refs` walks directories for rule and dashboard extensions only, so the Markdown files are named explicitly.
- **Fixed stale operator docs:** `docs/runbook.md`, `deploy/monitoring/README.md`, `README.md` and `.claude/` still described `service="audit-log"`, the removed deprecated gauges and the pre-v1.4.0 label sets.
- **`business.go`:** collectors are built in `buildCollectors` with `must(...)`, and one recover point (`build`) turns a constructor error into `Register`'s error. This replaces one `if err != nil` per collector, and is tested once (`TestBuild_MustAbortsWithError`).

### Changed — platform-gincommon v1.6.0 and full gincommon API adoption (2026-10-03)
- **Shared libraries at their latest release:** `platform-gincommon` v1.4.0 → **v1.6.0**. `platform-events/v2` v2.0.0 and `platform-pgcommon/v2` v2.0.0 were already the latest.
- **Tier 2 from the registry:** gincommon v1.5.0 adds `iam_rls_violations_total{violation_type}` to the central Platform Observability Registry (status `proposed`). The collector is now built with `gincommon.NewSharedCounterVec`, as in iam-org-membership, instead of `prometheus.NewCounterVec`. The ledger (`registry.go`, `metric-registry.yaml`) says `proposed`, and `IAMAuditLogCrossTenantAccess` / `IAMAuditLogMissingGUC` carry `metric_status: proposed`. `TestRegistryLedger_MatchesPlatformRegistry` now also checks the Tier-2 ledger. The scrape test no longer accepts a registry finding for it.
- **Tier 3 through gincommon's validating constructors:** every `iam_audit_log_*` collector is built with `gincommon.NewCounterVec` / `NewCounter` / `NewGauge` / `NewHistogramVec`. These check the prefix, suffixes and prohibited labels, and inject the identity const labels. The one exception is the `iam_audit_log_catalog_plans_stale_seconds` GaugeFunc, which gincommon has no constructor for. Series names, labels and buckets are unchanged. `check-metric-naming.sh` checks the new shape.
- **No direct OpenTelemetry/promhttp imports left:** `outbound/telemetry` wraps gincommon's own `NewSpanTracer`, `SpanTraceID` and `MetricsHandler`, so `/metrics` now continues past a failing collector instead of blanking the scrape. `check-observability-confinement.sh` forbids OTel and promhttp imports everywhere, the telemetry adapter included (gap 43's seam is retired).
- **Reconciler metrics identity:** the reconciler's metrics now carry `service="iam-audit-log"` (`MetricsServiceName`), like iam-org-membership's reconciler, so its `iam_audit_log_*` collectors pass gincommon's prefix check. Its trace `service.name` stays `iam-audit-log-reconciler`. Its metrics are not scraped (D-21), so no rule or dashboard changes.
- **pgcommon v2 error helpers:** `wrapConnErr` classifies with `pgcommon.IsPoolClosed`, `IsOperatorIntervention`, `IsQueryCanceled` and `IsSystemError`, replacing the local SQLSTATE-57/58 string match and the `puddle.ErrClosedPool` check. This fixes a regression from the v2 migration: pgcommon v2 returns its own closed-pool error, which mapped to 500 instead of 503 `dependency_unavailable` (`TestPostgresAdapter_Integration`, `TestDatabaseOutage503`).
- **Tests, one metrics identity per process:** the two `APP_ENV=prod` startup tests (`cmd/server`, `cmd/reconciler`) run in a re-executed child process (`fixtures.InOwnProcess`), because gincommon rejects a second metrics identity in one process. They had panicked since the v1.4.0 migration.
- **Tracing init:** both roots use `gincommon.InitTracingWithConfig` with the metrics identity (service, version, `domain=iam`, `APP_ENV`) and the gincommon logger, replacing `InitTracingFromEnv`. `OTEL_EXPORTER_OTLP_ENDPOINT=none` or `OTEL_TRACES_EXPORTER=none` turns export off (v1.6.0).

### Fixed — trace service name and dependency CVEs (2026-09-28)
- **Helm: `OTEL_SERVICE_NAME` is now set per composition root** (`serverEnv` `iam-audit-log`, `reconcilerEnv` `iam-audit-log-reconciler`, matching each root's `cfg.ServiceName`). Without it, gincommon's `InitTracingFromEnv` fell back to `APP_NAME`, which the chart never set, and then to `"platform-gincommon"`. Every span would have carried the library's name as its `service.name`.
- **Dependencies (govulncheck):** `google.golang.org/grpc` v1.83.0 → v1.83.2 fixes GO-2026-6348, which the service calls: an HTTP/2 DATA-frame memory exhaustion reached through the gincommon OTel exporter. The same bump fixes GO-2026-6443, which it does not call. `golang.org/x/crypto` v0.54.0 → v0.56.0 fixes GO-2026-6355, -6354 and -6303, none of them called. GO-2026-5932 (x/crypto) has no fix yet and is not called (c62cf3e).

### Changed — dependencies upgraded to latest (2026-09-28)
- **AWS SDK v2:** core v1.47.1, `config` v1.33.6, `credentials` v1.20.6, `glue` v1.164.0, `s3` v1.113.4, `sns` v1.47.2 and `sqs` v1.52.1, plus `smithy-go` v1.28.2.
- **Other direct dependencies:**
  - `pgx/v5` v5.11.0;
  - `otel` and `otel/trace` v1.46.0;
  - `prometheus/client_model` v0.6.3;
  - `jsonschema/v6` v6.0.3;
  - `testify` v1.12.1;
  - `x/time` v0.16.0.
- **Tooling:** golangci-lint v2.14.0. Transitive dependencies were refreshed for every build tag.
- **pgx v5.11 adds `TypeMap()` to `pgx.Rows`.** `test/dbseed`'s `bufferedRows` now implements it; it's test-only code.
- **Already latest:** platform-events v1.4.0, platform-gincommon v1.3.0 and platform-pgcommon v1.3.0. gin, uuid, puddle, client_golang, swag, testcontainers and yaml.v3 had no newer release.

### Changed — developer tooling (2026-09-28)
- **CI Trivy gate:** new `.trivyignore`, wired into `ci.yml`'s table and SARIF scans. Its one entry is DLA-4792-1 (`tzdata` 2026b → 2026c in the distroless base, UNKNOWN), expiring 2026-10-31. Upstream `:nonroot` has not been rebuilt yet, so `make pin-base-images` changes nothing, and the service loads no time zone. Every entry must carry a reason and an `exp:` date.
- **`Makefile`:** mirrors iam-org-membership's structure. Adds `godoc`, `test-smoke`, `prometheusrule` and `docs-serve`, runs a parallel `make test`, and prints a summary of failing postgres tests (06c65e5).
- **`.env-example`:** mirrors iam-org-membership's sectioned layout. Adds `APP_NAME` and `OTEL_SERVICE_NAME`, plus the OTel exporter and pool-tuning settings with notes on which library reads each. `PG_BOUNCER_MODE` changes from `false` to `true` to match the chart; both pools force it on regardless.

### Changed — metrics aligned with the Enterprise Platform Observability Standard (gap 46; e578208, 2026-09-28)
- **Tier 1:** the full Canonical `platform_*` set, with the ratified label vocabulary (`queue`, `event_type` bounded to the taxonomy, `reason`, `dependency`, `operation`, `outcome`). Adds `platform_retry_total`, `platform_duplicate_messages_total`, `platform_event_propagation_seconds`, `platform_queue_depth` and `platform_dlq_depth`. `platform_dependency_request_seconds` is relabelled to `{dependency,operation,outcome}` and now also times S3.
- **Labels:** `service="audit-log"` and `domain="iam"` are injected centrally on every series.
- **Tier 2:** `iam_rls_violations_total` is now fed (migration `000010`, `audit_rls_violation_counts()`, exactly once fleet-wide).
- **Tier-3 gauge renames, compatibility period:** `default_partition_rows_total` → `default_partition_rows`, `archive_stalled` → `archive_stalled_partitions`, and `iam_audit_log_dlq_messages_total` → `platform_dlq_depth`. The old names are still emitted, marked deprecated.
- **Registry and CI:** `deploy/monitoring/metric-registry.yaml` (inventory and label vocabulary), `metric-lint.yaml`, `metrics/registry.go`, and `test/unit/metrics_registry_test.go`. `TestAlerts_ReferenceKnownMetrics` now covers every artifact and bans deprecated names.
- **Artifacts:**
  - alerts migrated to the new names, with new failure-ratio, retry-spike, backlog, dead-lettered and dependency-error alerts;
  - new `recording-rules.yml`; `slo-rules.yml` for ingest lag, propagation, availability and DLQ, with burn-rate alerts;
  - new `dashboard-audit-log.json` and `prometheus-adapter-rule.yaml`;
  - the HPA has an optional `targetQueueDepthPerReplica`;
  - the Helm `PrometheusRule` is generated by `scripts/gen-prometheusrule.py`.
- Runbook, `deploy/monitoring/README.md`, LLD §11 and §25 (rev 0.26) and the `.claude` docs are updated.

### Changed — platform-library confinement (gaps 43–45; e578208, 2026-09-28)
- **Logs, metrics and traces go only through platform-gincommon (gap 43):**
  - New `internal/adapter/outbound/telemetry` is the single OTel/promhttp seam: `NewTracer`, `TraceID`, `MetricsHandler`, `HTTPErrorLog`, `QuietGin`.
  - `/metrics` serves exactly gincommon's registry.
  - Both roots build the gincommon logger first, so config and job-name errors are logged through it.
  - `SlogStyleLogger` no longer falls back to `log/slog`.
  - net/http error lines are forwarded to the gincommon logger, and gin writes nothing itself.
  - `core/port` imports no vendor package.
  - Enforced by the new `check-observability-confinement.sh` (in `make invariant-lint`).
- **Database only through platform-pgcommon (gap 44):**
  - `test/dbseed` drops its `pgxpool.ParseConfig`.
  - The `arch-lint.sh` database invariant now also scans `cmd/`, and forbids raw `Begin`/`Acquire`, direct `Connect`/`ParseConfig`, `pgconn.PgError` classification in production, hand-built `pgcommon.Config{}`, and `PG_*` reads.
- **Events only through platform-events (gap 45):**
  - Consumers are built by `events.NewSQSConsumer`, so the library owns the SQS client.
  - Every `SQS_*` setting now comes from platform-events `config.LoadSQS` (new `SQS_CONCURRENCY=4`); the service's own reads are removed.
  - The dead-letter threshold is `DeadLetterObserveAt(SQS_MAX_RECEIVE_COUNT)`.
  - Dedup stays keyed on `Envelope.ID` in the single `processed_events` writer.
  - Enforced by `check-forbidden-events-bypass.sh` §4.
- `TestReconcilerBinary_RejectsUnknownJob` now asserts the structured gincommon log line.

### Added — Phase 8: observability & hardening (b64f932, 2026-09-27)
- **D-21:** `cmd/server` exports DB/SQS-derived alert gauges every `OPS_STATS_INTERVAL`, from `audit_ops_stats()` (migration `000009`, aggregate numbers only) plus DLQ depth: `archive_stalled`, `archive_lag_seconds`, `default_partition_rows_total`, `redaction_pending_tasks`, `dlq_messages_total{queue}`.
- **Full LLD §11 Tier-3 set:**
  - write path: ingested, duplicates, unknown, ingest lag, direct-write outcomes;
  - read path: plan-window clamps, archived reads, export lifecycle.
  Caller-supplied label values are bounded.
- **Alerts:** a Helm `PrometheusRule` plus the static `deploy/monitoring/app-alerts.yml`, 19 rules, each with a `runbook_url`.
- **Runbook:** `docs/runbook.md` covers RB-1..RB-10.
- **Contract tests:** LLD §11 ↔ registry, alerts ↔ metrics, static ↔ Helm, and an AST log-hygiene check.
- **Fixed:** schema-validation errors no longer quote payload values into logs.

### Added — Phase 7: reconciler (c96ad3c, 2026-09-27)
- `reconcile` archives each retained tier to per-tenant `jsonl.gz` parts (SSE-KMS, Object Lock retain-until = max_occurred_at + tier), verifies checksums, the manifest and the lock, and drops the partition only through `audit_drop_partition()` (the AL-INV-9 gate).
- Automatic re-open of dropped months with late rows (`audit_reopen_partition()`, D-19).
- A `sealed` manifest flag with sealed-only archive routing (D-20). Redaction invalidates stale uploaded archives.
- The `processed-events-prune` job, with its CronJob enabled.
- **Fixed:** the reconciler reads partitions through the parent (grants don't extend to partitions); the DLQ wire test was isolated from shared-queue leftovers.

### Added — Phases 5–6: CAT-I2 poller, AL-7, GDPR redaction (612ccd6, 2026-09-26)
- **Poller:** the CAT-I2 `PlanPoller`, stale-if-error, swapping the map only on a `record_versions` change (AL-D15, D-13).
- **AL-7:** `GET /api/v1/internal/audit/events` (mesh-only, no plan clamp; `422 range_too_large`, D-14).
- **Immediate redaction:** `UserDeleted` → a task in the same transaction → `apply_redaction()` (D-1, D-15). `missed` is detected via the manifest's `subject_ids` (D-17) and treated as routine under AL-Q15 Option A.
- **Late rows (D-18):** the `redacted_subjects` ingest-time check, plus the `redaction-retry` and `redaction-sweep` jobs.
- **Fixed:** the redaction task insert uses a target-less `ON CONFLICT` (`audit_app` has INSERT only).

### Added — Phase 4: query + export (9d62133, 2026-09-26)
- **AL-1/AL-2:** a keyset query clamped to the plan window, merging hot rows with archived S3 objects. An oversized archived range becomes a `202` export (D-10, D-12). AL-2 has an archive fallback by id range.
- **AL-3/AL-4:** exports through an in-server worker (`claim_export_job()`, D-2), with SSE-KMS objects and a fresh presigned URL on every poll (D-11).
- Per-object archive manifest `audit_archive_objects` (migration `000006`).

### Added — Phases 0–3 (13c96bd)
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

### Fixed — Phases 0–3 (13c96bd)
- **AL-INV-3:** the audit_app pool now forces pgcommon's PgBouncer
  (transaction-local) GUC path. With `PG_BOUNCER_MODE=false` (the dev
  default), pgcommon set `app.tenant_id` session-level in a PrepareConn
  hook, so one tenant's GUC survived COMMIT and leaked to the next checkout
  of that backend. Caught by `test/postgres` TestGUC_*.
- `cmd/reconciler`: `--job` used the global flag set, so a second `run()` in
  one process panicked ("flag redefined"); now a local `FlagSet`.
- `cmd/server`: platform-events / pgmetrics metric init is guarded by
  `sync.Once` (a repeated init panicked on duplicate registration).
