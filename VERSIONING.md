# Versioning — iam-audit-log

SemVer on the **runtime contract** below; tags `vMAJOR.MINOR.PATCH` drive
`release.yml` (prod deploys pass the `production-data-migrations` gate because
the server self-migrates the `audit` DB — LLD §4.4).

| Bump | When | Examples |
|---|---|---|
| **MAJOR** | Breaking change to a frozen contract | removing/renaming an AL-1..AL-7 path or request/response field; a `processed_events.consumer` value; an `entry_type` value; a required env var; shortening a retention tier (never allowed — AL-INV-6) |
| **MINOR** | Backward-compatible addition | a new `entry_type` / taxonomy row; a new optional field; a new consumed queue; a new metric |
| **PATCH** | No contract change | bug fixes, internal refactors, dependency bumps |

## What is contract (LLD §25 name inventory)

| Contract | Not contract |
|---|---|
| HTTP routes AL-1..AL-7, the `Idempotency-Key` requirement, frozen field names (§5.4) | `internal/*` package layout, Go identifiers — no other module imports this one |
| §17 error **codes** and their HTTP statuses | `message` wording |
| `entry_type` vocabulary and each type's `retention_tier` (§7.1) — tiers may only lengthen (AL-INV-6) | how the taxonomy is stored in code |
| Queue names and `processed_events.consumer` discriminators (§7.1, §7.5) | consumer concurrency, visibility-timeout tuning |
| Tables, enums, roles, RLS policy/function names (§4, §25) | index tuning |
| Tier-3 metric names `iam_audit_log_*` (§11) | label cardinality beyond what §11 documents |
| S3 archive/export key schemes (§15.4, §25) | object part sizes |

Consumed as a **container image + Helm chart**, never via `go get`. Producers
reach it at `http://iam-audit-log.iam.svc.cluster.local:8080` (AL-Q17).

## Release process

1. Update `CHANGELOG.md` (the `changelog-check` workflow enforces this for code/deploy changes).
2. Tag `vX.Y.Z` on `main`; `release.yml` builds both binaries + the image, signs it, and (if enabled) runs the deploy gate under `production-data-migrations`.
3. Notify producers (Catalog, Realm Provisioner, O&M, Group Mapping, Tender ACL) on any AL-5/AL-6 contract change, and Catalog per environment on first go-live (§18.2).
