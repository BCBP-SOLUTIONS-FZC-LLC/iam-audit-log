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

## IAM role (IRSA, Terraform)

- [ ] **`deploy/iam/policy.json`'s `ArchiveKMS` statement names the KMS key
      ARN** (`arn:aws:kms:<region>:<account>:key/<key-id>`), not the
      `alias/iam-audit-archive` placeholder, and `ACCOUNT_ID` is substituted
      everywhere. KMS authorizes `GenerateDataKey` / `Decrypt` against the
      key, so an alias ARN in `Resource` grants nothing.
      **If missing:** every SSE-KMS `PutObject` and `GetObject` is
      `AccessDenied`. Archival stalls (RB-2) and archived reads and exports
      return `503`.
- [ ] **The role may send to the 11 audit DLQs** (`sqs:SendMessage` on
      `*-audit-q-dlq`, policy statement `ForwardPoisonToOwnDLQ`, decision
      D-22). platform-events forwards malformed and undecodable messages there
      instead of deleting them.
      **If missing:** the consumer cannot forward; the message stays visible
      and SQS's own redrive moves it after `maxReceiveCount` (no loss, just
      slower), and `platform_messages_failed_total{reason="dead_letter_error"}`
      rises.

## Service mesh (Istio)

- [ ] **Istio (security.istio.io/v1, 1.22+) is installed and the `iam`
      namespace is meshed.** The chart renders an `AuthorizationPolicy` and a
      `PeerAuthentication` (`templates/mesh.yaml`) and labels the server pods
      for injection. `APP_ENV=prod` refuses to render without the policy unless
      `mesh.acknowledgeNoMesh=true` (a tracked security exception).
      **If missing:** the install fails on the unknown CRD kinds. Without the
      policy, any pod admitted by the NetworkPolicy can forge AL-5/AL-6
      entries or read any tenant through AL-7 with a header.
- [ ] **`mesh.authorizationPolicy.internalCallerPrincipals` and
      `networkPolicy.internalCallers` list exactly the real AL-5/AL-6/AL-7
      callers** (defaults: Catalog Admin, Realm Provisioner, Org & Membership, Group Mapping, Tender ACL,
      as `cluster.local/ns/iam/sa/<release>`). Confirm each caller's service
      account name.
      **If missing:** a caller not listed gets `403 RBAC: access denied` from
      the sidecar, and its entries wait in its own retry queue.
- [ ] **The gateway reaches `/api/v1/audit/*`.** With a non-injected Envoy
      Gateway (default) it sends plaintext, which the policy allows only from
      sources without a mesh principal. If the gateway is mesh-injected, set
      `mesh.authorizationPolicy.gatewayPrincipals` and
      `mesh.peerAuthentication.apiPortMode: STRICT`.
      **If missing:** tenant admins get `403` on AL-1..AL-4.

## Cluster

- [ ] The service is deployed in the `iam` namespace (AL-Q17).
      **If missing:** Catalog's same-namespace NetworkPolicy blocks the CAT-I2
      poller, which then serves the stale map indefinitely (RB-8).

## Operations (until Phase 8 metrics publishing)

- [ ] **Reconciler health is watched through CronJob state, not metrics** (gap
      40, LLD rev 0.23 §11). The archival metrics are internal only until
      Phase 8. Alert on failed Jobs for the `reconcile`, `redaction-retry`,
      `redaction-sweep` and `processed-events-prune` CronJobs, and keep Job
      history and logs available. A blocked (pending redaction) or stalled
      (unverified tier, persistent late writes) archival run exits non-zero.
      **If missing:** a stalled archive goes unnoticed and hot partitions
      grow past the hot window.
