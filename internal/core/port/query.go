package port

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
)

// ErrNotFound is returned by readers when a tenant-scoped row does not exist
// (or RLS hides it — indistinguishable by design).
var ErrNotFound = errors.New("not found")

// ErrObjectMissing is returned by ArchiveReader when an archive object named
// by the manifest is absent from S3.
var ErrObjectMissing = errors.New("archive object missing")

// AuditReader is the RLS-scoped read side of audit_events and its
// projections (LLD §5.4 AL-1/AL-2). Every method binds app.tenant_id to
// tenantID for its transaction.
type AuditReader interface {
	// Page returns up to limit hot rows in (occurred_at DESC, id DESC)
	// order within [from, to] matching f, strictly after `after`.
	Page(ctx context.Context, tenantID string, f domain.QueryFilter, from, to time.Time, after *domain.Cursor, limit int) ([]domain.AuditEntry, error)
	// Get returns one hot row by id (ErrNotFound if absent).
	Get(ctx context.Context, tenantID, id string) (domain.AuditEntry, error)
	// PlanWindow returns the tenant's tenant_plan_window row (nil if none).
	PlanWindow(ctx context.Context, tenantID string) (*domain.PlanWindowRow, error)
	// ArchivedObjects lists the manifest objects overlapping [from, to]
	// whose monthly partition no longer exists (decision D-12), newest
	// first. tier "" means every tier.
	ArchivedObjects(ctx context.Context, tenantID string, from, to time.Time, tier string) ([]domain.ArchiveObject, error)
	// ArchivedObjectsForID lists the dropped-partition manifest objects
	// whose [min_id, max_id] contains id and that hold rows at or after
	// from (AL-2 archived fallback).
	ArchivedObjectsForID(ctx context.Context, tenantID, id string, from time.Time) ([]domain.ArchiveObject, error)
}

// ArchiveReader streams one archive object's records (gzipped JSONL, D-10).
type ArchiveReader interface {
	ReadArchive(ctx context.Context, bucket, key string, fn func(domain.ArchiveRecord) error) error
}

// ExportStore uploads export objects and presigns download URLs (D-11).
type ExportStore interface {
	PutExport(ctx context.Context, key string, body io.ReadSeeker, size int64) error
	PresignExport(ctx context.Context, key string, ttl time.Duration) (string, error)
}

// ExportJobs is the audit_export_jobs repository. Tenant-scoped methods bind
// app.tenant_id to tenantID; Claim goes through the claim_export_job()
// SECURITY DEFINER function (decision D-2).
type ExportJobs interface {
	Create(ctx context.Context, job domain.ExportJob) (domain.ExportJob, error)
	Get(ctx context.Context, tenantID, id string) (domain.ExportJob, error)
	MarkExpired(ctx context.Context, tenantID, id string) error
	Claim(ctx context.Context, lease time.Duration) (*domain.ExportJob, error)
	Heartbeat(ctx context.Context, tenantID, id string) error
	Complete(ctx context.Context, tenantID, id, s3Key string, rows int64, urlExpiresAt time.Time) error
	Fail(ctx context.Context, tenantID, id, reason string) error
}
