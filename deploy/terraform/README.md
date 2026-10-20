# iam-audit-log — Terraform prerequisites

Apply these resources **before the first `helm install`**. They are in the
platform Terraform workspace, not in this repo's CI/CD.

| File | What it does | Must exist before |
|---|---|---|
| `postgres-roles.tf` | Grants `audit_migrator` membership in `audit_reconciler` and asserts role attributes. | Startup migrations (`000007` sets `OWNER TO audit_reconciler`). Without this the server refuses to start. |
| `s3-lifecycle.tf` | S3 lifecycle expiration on the `exports/` prefix. | AL-3/AL-4 export objects can accumulate forever without it. |

The S3 bucket itself (`iam-audit-archive`, Object Lock COMPLIANCE, cross-region
replication) is provisioned by the platform Terraform workspace, not here.

## KMS key ARN

`deploy/iam/policy.json` `ArchiveKMS.Resource` contains the placeholder
`arn:aws:kms:ap-south-1:ACCOUNT_ID:key/REPLACE_WITH_KMS_KEY_ID`.

Alias ARNs (`alias/…`) grant nothing in IAM identity policies. Substitute the
**actual key ARN** (`arn:aws:kms:<region>:<account>:key/<key-id>`) before
attaching the policy. Look it up with:

```bash
aws kms describe-key --key-id alias/iam-audit-archive \
  --query 'KeyMetadata.Arn' --output text
```

Then substitute `ACCOUNT_ID` and `REPLACE_WITH_KMS_KEY_ID` throughout.
