# iam-audit-log — Release Checklist

Deployment prerequisites before the first `helm install -n iam`. Each item
explains what breaks when it is missing. Background: `BUILD_PLAN.md` §C.

**Legend:** `[x]` = implemented in this repo (apply or verify); `[ ]` = cluster/AWS action required.

---

## Database roles (Terraform)

IaC: **`deploy/terraform/postgres-roles.tf`** — apply this in your platform
Terraform workspace before running migrations.

- [ ] **Apply `deploy/terraform/postgres-roles.tf`** (items below). This grants
      `audit_migrator` membership in `audit_reconciler` and creates/asserts all
      three runtime roles.

- [ ] **`audit_migrator` is a member of `audit_reconciler`** (gap 34; first
      needed by migration `000007`, Phase 6).
      `000007` runs `ALTER FUNCTION apply_redaction(uuid) OWNER TO
      audit_reconciler`. PostgreSQL only lets the migrating role hand ownership
      to a role it can `SET ROLE` to.
      **If missing:** startup migration fails (`must be able to SET ROLE
      "audit_reconciler"`), and the server does not start.

- [ ] **Runtime roles exist with the correct attributes:**
      `audit_app` (LOGIN, NOBYPASSRLS), `audit_reconciler` (LOGIN, BYPASSRLS),
      `audit_migrator` (LOGIN, BYPASSRLS, owns the schema) (LLD §4.3).
      **If missing:** server and reconciler refuse to boot.

---

## Archive / export bucket (S3)

IaC: **`deploy/terraform/s3-lifecycle.tf`** — apply in the platform Terraform
workspace that owns the `iam-audit-archive` bucket.

- [ ] **No default Object Lock retention on `AUDIT_ARCHIVE_BUCKET`** (gap 30).
      Archive objects receive per-object COMPLIANCE retention from the archiver.
      Verify: `aws s3api get-object-lock-configuration --bucket iam-audit-archive`.
      Expected: no default mode, or `Mode=COMPLIANCE` with `Days=0`.
      **If missing:** every `exports/` object is locked by the bucket default
      and cannot expire.

- [ ] **Apply `deploy/terraform/s3-lifecycle.tf`** — lifecycle expiration rule
      on the `exports/` prefix (default 7 days, ≥ `EXPORT_SIGNED_URL_TTL`).
      **If missing:** export objects accumulate indefinitely.

---

## IAM role (IRSA)

- [ ] **Substitute the real KMS key ARN** in `deploy/iam/policy.json`
      `ArchiveKMS.Resource`. The file currently contains the placeholder
      `arn:aws:kms:ap-south-1:ACCOUNT_ID:key/REPLACE_WITH_KMS_KEY_ID`. Alias
      ARNs (`alias/…`) grant nothing in IAM identity policies. Look up the ARN:
      ```bash
      aws kms describe-key --key-id alias/iam-audit-archive \
        --query 'KeyMetadata.Arn' --output text
      ```
      Replace `REPLACE_WITH_KMS_KEY_ID` with the returned key ID, and replace
      `ACCOUNT_ID` everywhere in the file.
      **If missing:** every SSE-KMS `PutObject`/`GetObject` returns
      `AccessDenied`; archival stalls (RB-2); archived reads and exports return
      503.

- [x] **`sqs:SendMessage` on the 12 audit DLQs** (`*-audit-q-dlq`) — statement
      `ForwardPoisonToOwnDLQ` in `deploy/iam/policy.json`. platform-events
      forwards malformed/undecodable messages there (D-22). The wildcard ARN
      covers all 12 queues including `entitlement-audit-q-dlq` (CD-21).
      **Action needed:** attach the policy to the IRSA role and substitute
      `ACCOUNT_ID` before deploying.

---

## Service mesh (Istio)

- [ ] **Istio (security.istio.io/v1, 1.22+) is installed and the `iam`
      namespace is meshed** (`istio-injection: enabled` label or revision label).
      The chart renders an `AuthorizationPolicy` + `PeerAuthentication`
      (`templates/mesh.yaml`) and requests sidecar injection. `APP_ENV=prod`
      refuses to render without the policy.
      **If missing:** `helm install` fails on unknown CRD kinds. Without the
      policy any pod admitted by the NetworkPolicy can forge AL-5/AL-6 entries
      or read any tenant through AL-7.

- [x] **`mesh.authorizationPolicy.internalCallerPrincipals` and
      `networkPolicy.internalCallers` are pre-populated** in `deploy/helm/values.yaml`
      with the five core IAM callers (Catalog Admin, Realm Provisioner, Org &
      Membership, Group Mapping, Tender ACL) using their conventional service
      account names.
      **Action needed:** confirm each name matches the actual `ServiceAccount`
      names in your cluster, then add Commercial Entitlements, Billing, and
      Platform cross-tenant jobs via `networkPolicy.additionalIngressRules` and
      `mesh.authorizationPolicy.additionalRules` once those services deploy.
      **If wrong:** a caller not listed gets `403 RBAC: access denied`; its
      entries wait in its own retry queue.

- [ ] **Enable the gateway route and supply gateway coordinates.** Set in
      `values.yaml` (or an override file):
      ```yaml
      ingress:
        enabled: true
        hosts:
          - host: <your-actual-hostname>   # e.g. api.company.com
            paths:
              - path: /api/v1/audit
                pathType: PathPrefix
        gateway:
          name: <gateway-name>             # e.g. platform-gateway
          namespace: <gateway-namespace>   # e.g. gateway
          sectionName: <listener-name>     # e.g. https
      ```
      If Envoy Gateway is mesh-injected also set:
      ```yaml
      mesh:
        authorizationPolicy:
          gatewayPrincipals:
            - cluster.local/ns/<gw-ns>/sa/<gw-serviceaccount>
        peerAuthentication:
          apiPortMode: STRICT
      ```
      **If missing:** `/api/v1/audit/*` is unreachable; tenant admins get
      connection refused or `404` from the gateway.

---

## Cluster

- [ ] **Deploy into the `iam` namespace** (`helm install -n iam …`).
      Catalog's same-namespace NetworkPolicy controls the CAT-I2 plans poll
      egress. Deploying in a different namespace isolates the pod from that
      NetworkPolicy and the plan poller falls back to
      `AUDIT_DEFAULT_QUERY_WINDOW_DAYS` indefinitely (RB-8).

---

## Operations

- [x] **Reconciler CronJob failure alerts are implemented** in
      `deploy/monitoring/app-alerts.yml`:
      - `IAMAuditLogReconcileJobFailed` (critical) — fires when any
        `iam-audit-log-reconcile-*` Job exits non-zero.
      - `IAMAuditLogMaintenanceJobFailed` (warning) — fires when
        `redaction-retry`, `redaction-sweep` or `processed-events-prune` fails.
      Both link to [RB-9](../../docs/runbook.md#rb-9--reconciler-cronjob-failed--service-down).
      **Action needed:** ensure `prometheusRule.enabled: true` (default) and
      that the `PrometheusRule` is picked up by your Prometheus operator
      (check `prometheusRule.additionalLabels`).

---

## Summary

| # | Item | Status | Action |
|---|---|---|---|
| 1 | `audit_migrator` member of `audit_reconciler` | Terraform IaC ready | `terraform apply deploy/terraform/postgres-roles.tf` |
| 2 | Runtime roles with correct attributes | Terraform IaC ready | Same apply |
| 3 | S3 bucket: no default Object Lock | Platform Terraform | Verify with `aws s3api get-object-lock-configuration` |
| 4 | S3 `exports/` lifecycle expiration | Terraform IaC ready | `terraform apply deploy/terraform/s3-lifecycle.tf` |
| 5 | KMS key ARN in `policy.json` | **Code fix done** (placeholder → key ARN format) | Substitute real key ID before attaching |
| 6 | DLQ `SendMessage` in `policy.json` | **Done** in repo | Substitute `ACCOUNT_ID` and attach policy |
| 7 | Istio 1.22+ installed, `iam` namespace meshed | External cluster ops | Platform/infra team |
| 8 | Mesh principals populated in `values.yaml` | **Done** (5 callers pre-populated) | Confirm service account names; add Commercial callers later |
| 9 | Gateway ingress configured | **Values ready**, coordinates needed | Set `ingress.enabled`, hostname, gateway name/namespace |
| 10 | Deploy in `iam` namespace | Helm flag | `helm install -n iam …` |
| 11 | Reconciler CronJob alerts | **Done** in `app-alerts.yml` | Confirm `prometheusRule.enabled: true` and operator labels |
