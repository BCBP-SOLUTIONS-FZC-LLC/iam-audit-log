# Versioning and releases

This repository is a **deployed Go microservice**, not a library other services `go get`. `pkg/requestctx` is an internal helper for this process, and the `go.mod` module path exists so this repo's own code compiles, not for import by sibling repos. This document differs from [`platform-events`](https://github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events)'s `VERSIONING.md` for that reason.

What gets versioned here is:
- the **container image** (`ghcr.io/bcbp-solutions-fzc-llc/iam-audit-log`);
- the **Helm chart** (`deploy/helm/`) that deploys it;
- the runtime contract that its dependants rely on.

Those dependants are the platform's audit producers (every IAM and domain service publishing to the 11 audited topics), the direct-write callers of AL-5/AL-6 (Realm Provisioner, Catalog / Admin Config, Group Mapping, Tender ACL, Org & Membership and others; LLD §18.2), tenant admins through the UI (AL-1..AL-4), compliance callers of AL-7, and operators. Versions are published with **Git tags** and described in [CHANGELOG.md](./CHANGELOG.md).

This document follows the same SemVer / image-tag / maintainer-process layout as [`iam-org-membership/VERSIONING.md`](https://github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/blob/main/VERSIONING.md), which is itself modeled on `iam-realm-provisioner`'s and `iam-authz-enrichment`'s.

## Semantic versioning (SemVer)

We use [SemVer 2.0.0](https://semver.org/): `MAJOR.MINOR.PATCH` (e.g. `v1.2.3`).

| Bump | When you change | Examples |
|------|-----------------|----------|
| **MAJOR** | Breaking change in the runtime contract (§ below) | Removing or renaming an AL-1..AL-7 route or a documented request/response field; removing the `Idempotency-Key` semantics; renaming a consumed queue or a `processed_events.consumer` value; removing or renaming an `entry_type`; **shortening a retention tier (never allowed, AL-INV-6)**; renaming a §17 error **code**; a new required env var; an incompatible `values.yaml` restructure; renaming or relabelling a Canonical metric without the compatibility period |
| **MINOR** | New backward-compatible capability | A new endpoint (a new AL-n, never reusing an id); a new optional env var or Helm value; a new consumed queue or taxonomy row (`entry_type`); a new `iam_audit_log_*` metric; an additive response field; a new reconciler job |
| **PATCH** | Backward-compatible fix | A bug fix (e.g. the gap-35 `ON CONFLICT` 42501 fix); a performance improvement; a dependency bump with no observable behavior change; a documentation-only correction |

### What counts as the runtime contract

This service has no Go package for another service to import. Its "public API" is the wire and deployment contract every producer, caller and operator depends on. LLD §25 (the name inventory, "proposed freeze") is the frozen-name registry, and the tables below follow it.

| In scope (SemVer applies) | Out of scope (may change without MAJOR) |
|---------------------------|------------------------------------------|
| The 7 routes across two prefixes, with method, path, and documented request/response field names (LLD §5.4, README § API overview): `/api/v1/audit/*` AL-1 `GET /events`, AL-2 `GET /events/:id`, AL-3 `POST /exports`, AL-4 `GET /exports/:id`; `/api/v1/internal/*` AL-5 `POST /audit-entries`, AL-6 `POST /audit-entries:batch`, AL-7 `GET /audit/events`. Also: AL-6's always-207 per-entry body (D-4), AL-1's `202` deferral (D-10), AL-7's `422 range_too_large` (D-14) | `internal/*` package structure, exported Go identifiers and file layout; no other module imports them. Gin's internal route-group structure is not part of the wire contract |
| The `Idempotency-Key` header semantics on AL-5/AL-6 (it becomes `source_event_id`; a replay returns `200` with the same id; AL-INV-4) and the per-entry `idempotency_key` on AL-6 (D-4) | How the replay is detected internally (the ledger vs the `uq_audit_events_source_id` backstop) |
| The **11 consumed queue names** (`*-audit-q` + `-dlq`, LLD §7.1 / §25) and their `processed_events.consumer` discriminators (`auth`, `user`, `membership`, `tenant`, `delegation`, `serviceaccount`, `tender`, `billing`, `usage`, `wf_workflow`, `wf_template`, plus `direct_write`); consumed event types, which are **receive-only** (`api/asyncapi.yaml`, zero send operations: AL-INV-10) | Consumer concurrency (`SQS_CONCURRENCY`), visibility-timeout tuning, the Glue decode internals |
| The **`entry_type` vocabulary and each type's `retention_tier`** (LLD §7.1, `internal/core/domain/taxonomy.go`). A tier may only lengthen, never shorten (AL-INV-6); unknown types persist as `<domain>.unknown` (AL-EVT-4) | How the taxonomy is stored in code; the normaliser's derivation of actor/target (D-8), so long as the persisted shape is unchanged |
| §17 error **codes** (`internal/core/domain/errors.go`, 13 codes, e.g. `unknown_entry_type`, `metadata_too_large`, `range_too_large`, `forbidden_peer`) and their HTTP statuses | Exact error `message` wording |
| Required env var **names and semantics** (`internal/config/config.go`; required outside dev: `DATABASE_URL`, `MIGRATION_DATABASE_URL` for `cmd/server`, and `RECONCILER_DATABASE_URL` for `cmd/reconciler`), plus `AUDIT_ARCHIVE_*`; the library-owned `PG_*` / `SQS_*` / `OTEL_*` names follow their libraries | Env var **defaults** (`AUDIT_DEFAULT_QUERY_WINDOW_DAYS=365`, `ARCHIVE_SYNC_MAX_ROWS=10000`, `EXPORT_JOB_LEASE=15m`, `OPS_STATS_INTERVAL=60s`, etc.): tunable without a MAJOR bump, unless the new default itself breaks a documented invariant |
| Database objects consumers and operators rely on (LLD §4, §25): tables, enums, roles (`audit_app`, `audit_reconciler`, `audit_migrator`, `admin_readonly`), RLS policy / function names; the migration sequence `000001`–`000010` is append-only | Index tuning; the internals of definer function bodies, so long as their name, signature and grants are unchanged |
| The **S3 key schemes** (LLD §15.4, §25): archive `{retention_tier}/{tenant_id}/{yyyy}/{mm}/audit_events_{yyyy}_{mm}-part-NNNN.jsonl.gz` (D-10), export `exports/{tenant_id}/{export_id}.jsonl.gz`, and the gzipped-JSONL record shape | Object part sizes (`ARCHIVE_PART_MAX_ROWS`) |
| `deploy/helm/values.yaml` top-level key names and shapes consumers actually set (`image.*`, `env`, `serverEnv`, `reconcilerEnv`, `secrets.*`, `cronjobs.*`, `autoscaling.*`, `prometheusRule.*`, `serviceMonitor.*`, `networkPolicy.*`, replica fields) | Chart internals (`_helpers.tpl`, template structure) not exposed as a `values.yaml` key |
| Prometheus metric **names, label sets and label vocabularies** under the Enterprise Platform Observability Standard (`deploy/monitoring/metric-registry.yaml`, `metrics/registry.go`): the 10 Canonical Tier-1 `platform_*` series, Tier-2 `iam_rls_violations_total`, and the Tier-3 `iam_audit_log_*` series. A rename follows the standard's compatibility period: old and new emitted in parallel, the old marked `deprecated` with `replaced_by` / `sunset`, and removed only after the sunset (a MINOR bump while in parallel, and removal in a MAJOR bump, or after the approved sunset) | Histogram bucket boundaries; recording-rule names; dashboard layout |
| `GET /healthz` / `GET /readyz`: existence and meaning (`/readyz` checks Postgres and every SQS consumer; `503` otherwise) | `GET /asyncapi` and `/asyncapi.yaml` content shape (documentation surfaces, not a contract a caller must hold stable); `METRICS_PORT`'s default (a Helm default, not a SemVer surface) |

AL route ids are **permanently assigned**: a removed AL-n is never reused for a new endpoint. Reusing a retired id would itself be a breaking, confusing change, even though the id is technically free.

This service holds **no third-party admin-API client** to track in a compatibility row. Its one synchronous outbound call is the platform's Catalog CAT-I2 (`GET /api/v1/internal/plans`), an internal platform-service dependency tracked by Catalog's own releases. S3, SQS and Glue are reached through the AWS SDK behind the platform libraries.

### Guarantees

- **Pre-`v1.0.0` (current status):** per [SemVer §4](https://semver.org/#spec-item-4), anything may change at any time while the major version is `0`. This service has **never been deployed** to any environment (no Git tag, no live deployment) and has not yet committed to a stable *service* contract through a tag. That is why, for example, the gap-46 metric relabelling could be applied directly without a compatibility period on unchanged names. The table above still indicates what is *more* disruptive than what within `v0.x`: a MINOR bump is still meant to signal "safer than a MAJOR bump would have been". But a producer or caller should not yet assume `v0.x` compatibility across a MINOR bump the way it could once `v1` ships.
- **MAJOR (once `v1` ships):** we avoid breaking changes to the runtime contract within `v1.x`. A breaking change ships as `v2.0.0` with migration notes in the CHANGELOG. Retention tiers never shorten, in any version (AL-INV-6).
- **MINOR:** safe to redeploy without changing producer topics, caller URLs, or Helm values, unless you opt into a new capability.
- **PATCH:** drop-in image replacement; upgrade recommended for security fixes (every image is CVE-scanned, see below).

## Supported releases

| Version | Status | Image tag | Notes |
|---------|--------|-----------|-------|
| *(none tagged)* | **Unreleased** | — | [CHANGELOG.md](CHANGELOG.md) is entirely `[Unreleased]` (Phases 0–8 plus the platform-library and observability-standard alignment). The Helm chart `version` is `0.1.0` and `appVersion` is `"1.0.0"`. Bump both in lockstep with the first Git tag, and set `appVersion` to the tagged version: do not treat the current `appVersion` as a published release. No live deployment yet, so no support window has started |
| `< v0.1.0` | — | — | No tagged Git releases |

**This service is not yet safe to release even once `make ci` is green.** First complete the deployment prerequisites in [`docs/implementation/RELEASE_CHECKLIST.md`](docs/implementation/RELEASE_CHECKLIST.md):
- Terraform grants `audit_migrator` membership in `audit_reconciler`; without it, migration `000007` fails at startup (gap 34);
- the archive bucket has no default Object Lock retention, and has an `exports/` lifecycle expiry (gap 30);
- the service is deployed in the `iam` namespace, so the CAT-I2 poller can reach Catalog;
- alerting is on CronJob failures (gap 40).

Two open items sit upstream in the platform libraries and do not block a release (LLD AL-Q19): the platform-events malformed-envelope behavior (D-3, gap 42), and the missing gincommon span/metrics/trace-id APIs (gap 43). Legal still has to confirm AL-Q15 Option A.

This service has no formal support-window policy yet, since nothing runs in production against a tagged release. Once a `v1.0.0` ships to a real environment, this section will define how long a superseded major line receives security-only fixes. Expect the same platform convention `platform-events` and the IAM siblings use: security fixes only, for a period the platform team sets, typically about 6 months after the next major.

## Consume a release

This service is **not** consumed via `go get`; do not add this module as a dependency of another Go service. It is consumed as a **container image** on the mesh (public `/api/v1/audit/*` behind the gateway, internal `/api/v1/internal/*` mesh-only), deployed with the **Helm chart** in `deploy/helm/`, at `http://iam-audit-log.iam.svc.cluster.local:8080` (AL-Q17). Producers integrate by publishing to their own audited SNS topic, which this service subscribes to (LLD §18.1), or by calling AL-5/AL-6 (§18.2).

### Pull and verify the image

Once a tag exists:

```bash
docker pull ghcr.io/bcbp-solutions-fzc-llc/iam-audit-log:v0.1.0
```

**Today** only `main`-branch images are published (see CI below). Every image pushed from `ci.yml` is signed keylessly via Sigstore/Cosign (no long-lived key). Verify before deploying:

```bash
# main-branch builds (current: signed by ci.yml's push job)
cosign verify \
  --certificate-identity-regexp "^https://github\.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/.github/workflows/.*@refs/heads/(main|master)$" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/bcbp-solutions-fzc-llc/iam-audit-log@<digest>
```

Tagged releases, published by `.github/workflows/release.yml`, use `@refs/tags/` in the identity regexp instead of `@refs/heads/(main|master)`:

```bash
# tagged releases (signed by release.yml's docker job)
cosign verify \
  --certificate-identity-regexp "^https://github\.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/.github/workflows/.*@refs/tags/.*$" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/bcbp-solutions-fzc-llc/iam-audit-log@<digest>
```

See [README.md § CI](README.md#ci).

### Image tag scheme

`release.yml`'s `docker/metadata-action` step produces, per tag push (the same scheme as `iam-org-membership` / `iam-realm-provisioner` / `iam-authz-enrichment`):

| Tag pattern | Produced for | Example (from `v1.2.3`) |
|-------------|--------------|--------------------------|
| `vMAJOR.MINOR.PATCH` | Every release | `v1.2.3` |
| `vMAJOR.MINOR` | Every release | `v1.2` |
| `vMAJOR` | Every release | `v1` |
| `latest` | Stable releases only (no pre-release suffix) | `latest` |

Separately, `ci.yml` pushes the image on every merge to `main` with branch and `sha-<short>` tags. Pin production by a **release tag or digest**, not `latest` or an untagged `main` build.

| Pin style | Use when |
|-----------|----------|
| `vMAJOR.MINOR.PATCH` | Production; exact reproducibility (recommended; also enables digest verification) |
| `vMAJOR.MINOR` | Accept PATCH updates automatically |
| `vMAJOR` | Accept MINOR/PATCH updates automatically; not recommended before `v1.0.0` |
| `latest` / untagged `main` | Local/dev experiments only; never production |

### Deploy via Helm

```bash
helm upgrade iam-audit-log ./deploy/helm \
  --install \
  --namespace iam \
  --set image.tag=v0.1.0 \
  --set secrets.server.existingSecret=iam-audit-log-server-secrets \
  --set secrets.reconciler.existingSecret=iam-audit-log-reconciler-secrets
```

`image.tag` (`deploy/helm/values.yaml`) falls back to the chart's own `appVersion` (`Chart.yaml`) when unset. Chart `version`/`appVersion` are bumped manually alongside the Git tag (see the maintainer process below); no workflow derives them from the tag today. **Each composition root has its own Secret** (implementation rule 5): the server's never contains `RECONCILER_DATABASE_URL`, and the reconciler's never contains `DATABASE_URL`. `test/unit/deploy_contract_test.go` enforces this.

The chart deploys **one** `Deployment` (`iam-audit-log`: the HTTP API on both route prefixes, the 11-queue consumer fleet, the CAT-I2 plan poller, the export worker and the ops monitor) plus **4** `CronJob`s:

| CronJob | Schedule |
|---|---|
| `reconcile` | `0 2 * * *` |
| `redaction-retry` | `*/15 * * * *` |
| `redaction-sweep` | `30 3 * * *` |
| `processed-events-prune` | `0 3 * * *` |

All run from the same image, which carries both the `iam-audit-log-server` (`ENTRYPOINT`) and `iam-audit-log-reconciler` binaries. The chart also renders the `PrometheusRule` (alerts, recording rules and SLOs, generated from `deploy/monitoring/` by `scripts/gen-prometheusrule.py`), the `ServiceMonitor`, and an optional HPA.

## Maintainer release process

`.github/workflows/release.yml` implements the validate → build → docker (CVE scan/sign) → **deploy-gate** → publish pipeline, modeled on the IAM siblings. Know before you tag that **the deploy-gate here is not optional**: the job fails immediately if the `KUBECONFIG_B64` secret isn't set, and the final `publish` job (the one that creates the GitHub Release) requires `deploy-gate` to have *succeeded*, not merely run. It runs under the **`production-data-migrations`** environment, because the server self-migrates the `audit` DB at startup (LLD §4.4), so a deploy **is** a migration. In practice, you cannot complete a tagged release of this service without a real cluster to deploy to.

When cutting a tagged release:

1. **Merge** all changes for the release to `main`. Confirm the deployment prerequisites in the Supported releases section above are met in the target environment before proceeding. Tagging over them ships a build whose first migration will fail.

2. **Update `CHANGELOG.md`:** move `[Unreleased]` entries into a new `## [X.Y.Z] - YYYY-MM-DD` section. `release.yml`'s `build` job runs `.github/scripts/verify-changelog-entry.sh`, which fails the release if this section is missing, so cut it *before* tagging.

3. **Bump `deploy/helm/Chart.yaml`'s `version`/`appVersion`** to match `X.Y.Z`. Nothing does this automatically: a consumer who deploys the chart without setting `image.tag` explicitly gets whatever `appVersion` was last committed, so a forgotten bump here silently ships a stale image.

4. **Run the gates locally** to confirm everything is green before tagging:
   ```bash
   make ci         # tidy + fmt-check + vet + lint + arch-lint + invariant-lint + test-ci (-race, 95% coverage gate) + build
   make test-e2e   # not part of make ci
   ```
   `make ci` includes `go-arch-lint` and every invariant script. The full run takes more than 10 minutes locally, so run it in the background.

5. **Create and push an annotated tag.** This is what triggers `release.yml`:
   ```bash
   git tag -a v0.1.0 -m "v0.1.0"
   git push origin v0.1.0
   ```

6. **The release workflow** (`.github/workflows/release.yml`) runs:
   - **`validate-test` / `validate-quality`:** the same reusable gates `ci.yml` uses, re-run at the exact tagged commit, including the **95% coverage gate** (`.github/scripts/coverage-gate.sh`). Merged coverage is about 96.5%.
   - **`build`:**
     - verifies that the tag matches HEAD (`verify-release-tag.sh`) and that the CHANGELOG entry exists (`verify-changelog-entry.sh`);
     - cross-compiles **both** binaries, `server` (`./cmd/server`) and `reconciler` (`./cmd/reconciler`), for 5 platforms (linux/darwin amd64+arm64, windows amd64) with `prepare-release-binary.sh`.
     The cross-platform binaries are a convenience for non-Docker runs; the primary distribution artifact is the container image.
   - **`docker`:** builds and pushes the semver-tagged image (see the tag scheme above), CVE-scans it (the release fails on CRITICAL/HIGH), generates a CycloneDX SBOM and SLSA provenance, signs with Cosign, then verifies the signature.
   - **`deploy-gate`** (requires `KUBECONFIG_B64`; **not skippable**, runs under `production-data-migrations`):
     - a live `helm upgrade --install --wait --atomic` with the per-root existing Secrets;
     - deployed-digest verification against what was pushed, then `kubectl rollout status`;
     - a Prometheus-backed error-rate check (`http_requests_total{status_class="5xx"}`, this service's own platform-gincommon HTTP metric) that auto-rolls back via `helm rollback` on failure.
     The error-rate check is skipped (not failed) if `PROMETHEUS_URL` isn't set, but the job as a whole still requires `KUBECONFIG_B64`.
   - **`publish`:** creates the GitHub Release with the CHANGELOG section as notes, plus the binaries, checksums, SBOM and provenance. Runs only if `build`, `docker` **and** `deploy-gate` all succeeded.

7. **Notify dependants** with upgrade notes if the release is MINOR or MAJOR:
   - the direct-write callers (Realm Provisioner, Catalog / Admin Config, Group Mapping, Tender ACL, Org & Membership);
   - the producers of the 11 audited topics;
   - compliance callers of AL-7 and the UI team owning AL-1..AL-4;
   - whoever owns the Helm deployment.

   Coordinate anything that touches an AL route, the `Idempotency-Key` semantics, an `entry_type` or tier, or a consumed queue name first. Those are the highest-blast-radius parts of this contract, given how many services write audit records here. Notify Catalog per environment on first go-live (LLD §18.2).

### Pre-release tags (optional)

| Tag pattern | Meaning |
|-------------|---------|
| `v1.1.0-rc.1` | Release candidate; not for production unless approved |
| `v1.1.0-beta.1` | Early integration testing |

Both match `release.yml`'s `v*` tag trigger and produce a signed, scanned image, but never a `latest` tag (see the tag scheme table above).

## Compatibility matrix

| iam-audit-log | Go (`go.mod`) | Shared platform libraries | Local AWS emulator |
|---|---|---|---|
| Unreleased (`main`) | `1.26.6` | `platform-gincommon` v1.3.0, `platform-events` v1.4.0, `platform-pgcommon` v1.3.0 | `floci/floci:2.1.0-compat` (Object Lock behavior pinned by `TestReconciler_FlociObjectLockSupport`, gap 16) |

None of those libraries is re-exported. A consumer of this *service* never needs them as a direct dependency of *this* module: producers and callers depend on the HTTP/event contract, not on any Go package here. Every log, metric and trace goes through platform-gincommon; every database connection, configuration and operation through platform-pgcommon; and every event consumption, SQS setting and dedup through platform-events (BUILD_PLAN gaps 43–45, CI-enforced). A bump of any of the three is therefore a change to review for contract impact, even though it is usually a PATCH. Runtime infrastructure (PostgreSQL 15 on RDS, S3 with Object Lock, SQS/SNS, Glue Schema Registry) is platform-provided and invisible to this matrix.

## Related files

| File | Purpose |
|------|---------|
| [CHANGELOG.md](./CHANGELOG.md) | User-facing history per version; currently entirely `[Unreleased]` |
| [README.md](./README.md) | Mental model, API overview, integration, local dev, CI/CD summary |
| [CONTRIBUTING.md](./CONTRIBUTING.md#deployment) | Deployment pipeline as it exists today (`ci.yml` / `release.yml`) |
| [docs/lld/iam-lld-audit-log-service.md](./docs/lld/iam-lld-audit-log-service.md) | The runtime contract in full: §5 API, §7 events and taxonomy, §12 configuration, §16 open-question register, §17 error taxonomy, §25 name inventory |
| [docs/implementation/BUILD_PLAN.md](./docs/implementation/BUILD_PLAN.md) | Build decisions D-1..D-21 and the spec-gap register (gaps 1–46) |
| [docs/implementation/RELEASE_CHECKLIST.md](./docs/implementation/RELEASE_CHECKLIST.md) | Deployment prerequisites that must be met before the first tagged release |
| [ARCHITECTURE.md](./ARCHITECTURE.md) | Layer model, flows, key invariants, threat model |
| [deploy/monitoring/metric-registry.yaml](./deploy/monitoring/metric-registry.yaml) | The metric contract: tiers, label vocabulary, deprecations with replacement and sunset |
| [.github/workflows/ci.yml](./.github/workflows/ci.yml) | Current image build / Trivy / Cosign-on-`main` pipeline |
| [.github/workflows/release.yml](./.github/workflows/release.yml) | Tag-triggered release pipeline (validate → build → docker → deploy-gate → publish) |
| [deploy/helm/Chart.yaml](./deploy/helm/Chart.yaml) | Helm chart version / app version |
| [go.mod](./go.mod) | Module path and minimum Go version; not a consumable package |
