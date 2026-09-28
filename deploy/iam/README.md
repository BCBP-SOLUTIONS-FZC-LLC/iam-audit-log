# IAM policy (IRSA) — iam-audit-log

`policy.json` is attached to the `iam-audit-log` IRSA role (LLD §13). No static
AWS keys are ever used.

- **SQS**: consume-only on the 11 `*-audit-q` queues; `GetQueueAttributes` on
  the DLQs for depth alerting. **No `sns:*` at all** — the service publishes
  nothing (AL-INV-10).
- **Glue**: read-only `GetSchemaVersion` / `GetSchemaByDefinition` (§7.3.1).
- **S3 / KMS**: put/get + Object Lock retention on `iam-audit-archive` (archive
  and exports, §15.4); explicit **Deny** on object deletion, governance bypass,
  and lock/lifecycle reconfiguration — tier expiry is the bucket lifecycle
  rule's job, bounded by Object Lock (§8.6, §10.4).

The bucket itself (Object Lock COMPLIANCE, per-tier lifecycle rules,
cross-region replication to the compliance-isolated account) is provisioned by
platform Terraform, not this repo. Note: `alias/...` in a KMS resource ARN is
illustrative — Terraform should substitute the key ARN.
