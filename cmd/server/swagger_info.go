// Package main is the entry point for the IAM Audit Log service.
//
// @title           IAM Audit Log API
// @version         0.1
// @description     Audit Log Service — the platform compliance system-of-record (LLD §1). Append-only, tenant-scoped audit trail.
// @description
// @description     **Public** `/api/v1/audit/*` (AL-1..AL-4): tenant_admin / tenant_owner only; RLS-scoped to x-tenant-id; plan-gated query window.
// @description     **Internal** `/api/v1/internal/*` (AL-5..AL-7): mesh-only, iam-system peers; direct-write ingest requires `Idempotency-Key`.
//
// @contact.name   BCBP Solutions
// @contact.email  indhu@bcbpsolutions.com
//
// @license.name   Proprietary
//
// @host      localhost:8080
// @BasePath  /api/v1
//
// @securityDefinitions.apikey TenantRoles
// @in                         header
// @name                       x-tenant-roles
// @description                Gateway/mesh-injected roles: tenant_admin|tenant_owner (public) or iam-system (internal).
//
// @securityDefinitions.apikey TenantID
// @in                         header
// @name                       x-tenant-id
// @description                Tenant UUID injected by the gateway / mesh caller.
//
// @securityDefinitions.apikey UserID
// @in                         header
// @name                       x-user-id
// @description                Caller subject (or the iam-system sentinel …00a1).
//
// @securityDefinitions.apikey IdempotencyKey
// @in                         header
// @name                       Idempotency-Key
// @description                Required on AL-5/AL-6; becomes the entry's source_event_id (LLD §5.4).
//
// @tag.name         Query
// @tag.description  Tenant audit search, single read, export (AL-1..AL-4)
//
// @tag.name         Ingest
// @tag.description  Mesh-only direct-write ingest (AL-5, AL-6)
//
// @tag.name         Internal
// @tag.description  Mesh-only provenance read (AL-7)
package main
