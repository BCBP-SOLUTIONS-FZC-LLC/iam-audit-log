# iam-audit-log — Release Checklist

Deployment prerequisites that live outside this repository: Terraform, AWS or
the bucket configuration. Each one must be confirmed before the release that
first needs it. Each item says what fails if it is missing. Background is in
`BUILD_PLAN.md` §C under the gap number given.

## Database roles (Terraform)

- [ ] **`audit_migrator` is a member of `audit_reconciler`** (gap 34; first
      needed by migration `000007`, Phase 6).
      `000007` runs `ALTER FUNCTION apply_redaction(uuid) OWNER TO
      audit_reconciler` (decision D-1). The function must run as
      `audit_reconciler` so the `audit_events` append-only trigger admits its
      `UPDATE`. PostgreSQL only lets the migrating role hand ownership to a
      role it can `SET ROLE` to.
      **If missing:** the migration fails at startup (`must be able to SET
      ROLE "audit_reconciler"`), and the server does not start. The schema
      itself is valid.
      ```hcl
      # e.g. with the cyrilgdn/postgresql provider
      resource "postgresql_grant_role" "migrator_can_own_reconciler_functions" {
        role       = "audit_migrator"
        grant_role = "audit_reconciler"
      }
      ```
- [ ] The runtime roles exist with the attributes asserted at startup:
      `audit_app` (LOGIN, NOBYPASSRLS), `audit_reconciler` (LOGIN, BYPASSRLS),
      `audit_migrator` (LOGIN, BYPASSRLS, owns the schema) (LLD §4.3).

## Archive / export bucket (S3)

- [ ] **No default Object Lock retention on `AUDIT_ARCHIVE_BUCKET`** (gap
      30). Archive objects get per-object COMPLIANCE retention from the
      archiver (Phase 7).
      **If missing:** every export object under `exports/` is locked for the
      bucket default and cannot expire.
- [ ] **A lifecycle expiration rule on the `exports/` prefix of at least
      `EXPORT_SIGNED_URL_TTL`** (7 days) (gap 30, decision D-11).
      **If missing:** export objects accumulate indefinitely.

## Cluster

- [ ] The service is deployed in the `iam` namespace (AL-Q17).
      **If missing:** Catalog's same-namespace NetworkPolicy blocks the CAT-I2
      poller, which then serves the stale map indefinitely (RB-8).
