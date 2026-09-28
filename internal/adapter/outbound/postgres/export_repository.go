package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
)

// ExportRepository implements port.ExportJobs on the audit_app pool
// (audit_export_jobs: SELECT/INSERT/UPDATE, RLS-scoped).
type ExportRepository struct {
	pool *pgcommon.Pool
}

var _ port.ExportJobs = (*ExportRepository)(nil)

// NewExportRepository returns a repository over the audit_app pool.
func NewExportRepository(pool *pgcommon.Pool) *ExportRepository {
	return &ExportRepository{pool: pool}
}

const (
	jobColumns = `id::text, tenant_id::text, requested_by::text, filter::text, status::text,
    s3_key, signed_url_expires_at, row_count, created_at, completed_at, error`

	insertJobSQL = `INSERT INTO audit_export_jobs (id, tenant_id, requested_by, filter)
VALUES ($1, $2, $3, $4::jsonb) RETURNING ` + jobColumns

	getJobSQL = `SELECT ` + jobColumns + ` FROM audit_export_jobs WHERE tenant_id = $1 AND id = $2::uuid`

	expireJobSQL = `UPDATE audit_export_jobs SET status = 'expired'
WHERE tenant_id = $1 AND id = $2::uuid AND status = 'ready' AND signed_url_expires_at <= now()`

	claimJobSQL = `SELECT job_id::text, job_tenant_id::text, job_requested_by::text, job_filter::text
FROM claim_export_job($1::interval)`

	heartbeatJobSQL = `UPDATE audit_export_jobs SET updated_at = now()
WHERE tenant_id = $1 AND id = $2::uuid AND status = 'running'`

	completeJobSQL = `UPDATE audit_export_jobs
SET status = 'ready', s3_key = $3, row_count = $4, signed_url_expires_at = $5, completed_at = now(), error = NULL
WHERE tenant_id = $1 AND id = $2::uuid AND status = 'running'`

	failJobSQL = `UPDATE audit_export_jobs SET status = 'failed', error = $3, completed_at = now()
WHERE tenant_id = $1 AND id = $2::uuid AND status = 'running'`
)

// Create implements port.ExportJobs.
func (r *ExportRepository) Create(ctx context.Context, job domain.ExportJob) (domain.ExportJob, error) {
	filter, err := marshalFilter(job.Filter)
	if err != nil {
		return domain.ExportJob{}, err
	}
	var out domain.ExportJob
	err = withPool(bindTenant(ctx, job.TenantID), r.pool, func(tx pgx.Tx) error {
		var err error
		out, err = scanJob(tx.QueryRow(ctx, insertJobSQL, job.ID, job.TenantID, job.RequestedBy, filter))
		return err
	})
	return out, err
}

// Get implements port.ExportJobs.
func (r *ExportRepository) Get(ctx context.Context, tenantID, id string) (domain.ExportJob, error) {
	var out domain.ExportJob
	err := withPool(bindTenant(ctx, tenantID), r.pool, func(tx pgx.Tx) error {
		var err error
		out, err = scanJob(tx.QueryRow(ctx, getJobSQL, tenantID, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return port.ErrNotFound
		}
		return err
	})
	return out, err
}

// MarkExpired implements port.ExportJobs.
func (r *ExportRepository) MarkExpired(ctx context.Context, tenantID, id string) error {
	return r.exec(ctx, tenantID, expireJobSQL, tenantID, id)
}

// Claim implements port.ExportJobs via claim_export_job() (decision D-2).
// It runs without a tenant binding: the definer function is the one
// deliberate cross-tenant step, and it returns only the claimed job.
func (r *ExportRepository) Claim(ctx context.Context, lease time.Duration) (*domain.ExportJob, error) {
	var out *domain.ExportJob
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		var (
			j      domain.ExportJob
			filter string
		)
		err := tx.QueryRow(ctx, claimJobSQL, fmt.Sprintf("%d seconds", int64(lease/time.Second))).
			Scan(&j.ID, &j.TenantID, &j.RequestedBy, &filter)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return nil
		case err != nil:
			return err
		}
		if err := json.Unmarshal([]byte(filter), &j.Filter); err != nil {
			return fmt.Errorf("decode export filter: %w", err)
		}
		j.Status = domain.ExportRunning
		out = &j
		return nil
	})
	return out, err
}

// Heartbeat implements port.ExportJobs (renews the claim lease).
func (r *ExportRepository) Heartbeat(ctx context.Context, tenantID, id string) error {
	return r.exec(ctx, tenantID, heartbeatJobSQL, tenantID, id)
}

// Complete implements port.ExportJobs.
func (r *ExportRepository) Complete(ctx context.Context, tenantID, id, s3Key string, rows int64, urlExpiresAt time.Time) error {
	return r.exec(ctx, tenantID, completeJobSQL, tenantID, id, s3Key, rows, urlExpiresAt)
}

// Fail implements port.ExportJobs.
func (r *ExportRepository) Fail(ctx context.Context, tenantID, id, reason string) error {
	return r.exec(ctx, tenantID, failJobSQL, tenantID, id, reason)
}

func (r *ExportRepository) exec(ctx context.Context, tenantID, sql string, args ...any) error {
	return withPool(bindTenant(ctx, tenantID), r.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, sql, args...)
		return err
	})
}

// marshalFilter serializes a filter for the jsonb column — as a string, never
// []byte (PgBouncer simple protocol, BUILD_PLAN gap 28).
func marshalFilter(f domain.QueryFilter) (string, error) {
	b, err := json.Marshal(f)
	if err != nil {
		return "", fmt.Errorf("encode export filter: %w", err)
	}
	return string(b), nil
}

func scanJob(row pgx.Row) (domain.ExportJob, error) {
	var (
		j              domain.ExportJob
		filter, status string
		s3Key, errText *string
	)
	if err := row.Scan(&j.ID, &j.TenantID, &j.RequestedBy, &filter, &status,
		&s3Key, &j.SignedURLExpiresAt, &j.RowCount, &j.CreatedAt, &j.CompletedAt, &errText); err != nil {
		return domain.ExportJob{}, err
	}
	if err := json.Unmarshal([]byte(filter), &j.Filter); err != nil {
		return domain.ExportJob{}, fmt.Errorf("decode export filter: %w", err)
	}
	j.Status = domain.ExportStatus(status)
	j.S3Key, j.Error = deref(s3Key), deref(errText)
	return j, nil
}
