# iam-audit-log

The Tender Management SaaS platform's **compliance system-of-record**: a single
append-only, tenant-scoped store answering *"who did what, to what, when, and
from where"* across every IAM and domain service.

- **Consumes** every audit-bearing SNS topic (11 SQS queues, catch-all, at-least-once, deduplicated) — and **publishes nothing** (AL-INV-10).
- **Direct-write ingest** (mesh-only): `POST /api/v1/internal/audit-entries[:batch]` for non-bus audit entries (AL-D1).
- **Admin query/export** (tenant_admin/owner): `GET /api/v1/audit/events`, exports with 7-day signed URLs; plan-gated query window.
- **Reconciler** CronJob: S3 archival (SSE-KMS, Object Lock COMPLIANCE), checksum verification, provable partition drop, tiered retention (7 y / 3 y / 90 d).

Spec: [`docs/lld/iam-lld-audit-log-service.md`](docs/lld/iam-lld-audit-log-service.md) ·
Build plan & trace: [`docs/implementation/BUILD_PLAN.md`](docs/implementation/BUILD_PLAN.md) ·
Architecture: [`ARCHITECTURE.md`](ARCHITECTURE.md)

## Quick start

```sh
make setup && make docker-up && make run
curl localhost:8080/healthz
```

Mesh address: `http://iam-audit-log.iam.svc.cluster.local:8080`.
