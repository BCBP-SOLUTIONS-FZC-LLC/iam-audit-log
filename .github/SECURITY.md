# Security Policy

## Supported Versions

| Version | Supported |
|---------|-----------|
| 0.x     | ✅ Active (pre-1.0, greenfield) |

Deploy the latest image.

## Reporting a Vulnerability

**Do not open a public GitHub issue for security vulnerabilities.**

Email: vijay@bcbpsolutions.com
Subject: `[iam-audit-log] Security vulnerability`

Include in your report:
- Description of the vulnerability and the affected component (query/export API, direct-write ingest, SQS consumer, reconciler job, RLS policy, role grant, S3 archive writer, etc.)
- Steps to reproduce
- Potential impact (cross-tenant audit read, forged audit history, mutation/deletion of an immutable record, premature archive deletion, PII in logs, DoS, etc.)
- Suggested fix or patch (if any)

### Response timeline

| Step | Target |
|------|--------|
| Initial acknowledgement | 48 hours |
| Severity assessment | 5 business days |
| Patch release (critical/high) | 14 days |
| Public disclosure | After patch ships |

We follow responsible disclosure. Reporters will be credited in release notes unless anonymity is requested.

## Scope

Areas of particular sensitivity in this service (the platform compliance system-of-record):

- **Append-only immutability (AL-INV-1)** — `audit_app` holds `INSERT`+`SELECT` only on `audit_events`; the `forbid_audit_mutation()` trigger is defense-in-depth. Any `UPDATE`/`DELETE` path for the app role is a critical finding.
- **Row-Level Security (AL-INV-3)** — fail-closed tenant isolation on `audit_events` / `audit_export_jobs`; `app.tenant_id` is bound transaction-locally via `IdentityBridgeMiddleware` → `pgcommon.WithGUCSet`. A bypass would expose another tenant's audit trail.
- **Direct-write forgery (§10.2)** — the ingest endpoint binds the body `tenant_id` as the GUC so `WITH CHECK` rejects cross-tenant writes; a bypass would let a producer forge another tenant's history.
- **Role separation** — the server (`audit_app`) and reconciler (`audit_reconciler`, BYPASSRLS, sole `DELETE`) never share credentials (separate Secrets).
- **Archive integrity (AL-INV-9)** — S3 Object Lock COMPLIANCE; a partition is dropped only after checksum-verified archival.
- **GDPR redaction (AL-INV-7/12)** — redaction touches only `security_3y` free-text metadata, never `compliance_7y` rows.
- **No outbound events (AL-INV-10)** — any SNS publisher/outbox wiring is a contract violation.
