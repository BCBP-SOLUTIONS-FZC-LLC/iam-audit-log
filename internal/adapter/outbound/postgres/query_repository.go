package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
)

// QueryRepository implements port.AuditReader on the audit_app pool. Every
// read runs under RLS with app.tenant_id bound to the caller's tenant; the
// explicit tenant_id predicate only lets the planner use the
// (tenant_id, occurred_at DESC) indexes — RLS is the isolation control.
type QueryRepository struct {
	pool *pgcommon.Pool
}

var _ port.AuditReader = (*QueryRepository)(nil)

// NewQueryRepository returns a reader over the audit_app pool.
func NewQueryRepository(pool *pgcommon.Pool) *QueryRepository {
	return &QueryRepository{pool: pool}
}

// bindTenant returns ctx with app.tenant_id bound to tenantID.
func bindTenant(ctx context.Context, tenantID string) context.Context {
	g, _ := pgcommon.GUCSetFromContext(ctx)
	g.TenantID = tenantID
	return pgcommon.WithGUCSet(ctx, g)
}

const entryColumns = `id::text, occurred_at, recorded_at, tenant_id::text, entry_type, action,
    actor_type::text, actor_id::text, actor_display, target_type, target_id,
    source_service, source_topic, source_event_type, source_event_id,
    retention_tier::text, ingest_mode::text, host(ip_address), user_agent, trace_id, metadata::text`

// pageSQL builds the keyset query for f. The parent table spans every
// attached partition; a month whose partition was dropped after archival
// simply contributes no rows here (decision D-12).
func pageSQL(tenantID string, f domain.QueryFilter, from, to time.Time, after *domain.Cursor, limit int) (sql string, args []any) {
	where := []string{"tenant_id = $1", "occurred_at >= $2", "occurred_at <= $3"}
	args = []any{tenantID, from, to}
	add := func(clause string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if len(f.EntryTypes) > 0 {
		add("entry_type = ANY($%d::text[])", f.EntryTypes)
	}
	if f.ActorID != "" {
		add("actor_id = $%d::uuid", f.ActorID)
	}
	if f.ActorType != "" {
		add("actor_type = $%d::audit_actor_type", f.ActorType)
	}
	if f.TargetType != "" {
		add("target_type = $%d", f.TargetType)
	}
	if f.TargetID != "" {
		add("target_id = $%d", f.TargetID)
	}
	if f.SourceService != "" {
		add("source_service = $%d", f.SourceService)
	}
	if f.RetentionTier != "" {
		add("retention_tier = $%d::audit_retention_tier", f.RetentionTier)
	}
	if after != nil {
		args = append(args, after.OccurredAt, after.ID)
		where = append(where, fmt.Sprintf("(occurred_at, id) < ($%d::timestamptz, $%d::uuid)", len(args)-1, len(args)))
	}
	args = append(args, limit)
	return fmt.Sprintf("SELECT %s FROM audit_events WHERE %s ORDER BY occurred_at DESC, id DESC LIMIT $%d",
		entryColumns, strings.Join(where, " AND "), len(args)), args
}

// Page implements port.AuditReader.
func (r *QueryRepository) Page(ctx context.Context, tenantID string, f domain.QueryFilter, from, to time.Time, after *domain.Cursor, limit int) ([]domain.AuditEntry, error) {
	sql, args := pageSQL(tenantID, f, from, to, after, limit)
	var out []domain.AuditEntry
	err := withPool(bindTenant(ctx, tenantID), r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e, err := scanEntry(rows)
			if err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

const getEntrySQL = `SELECT ` + entryColumns + ` FROM audit_events WHERE tenant_id = $1 AND id = $2::uuid LIMIT 1`

// Get implements port.AuditReader.
func (r *QueryRepository) Get(ctx context.Context, tenantID, id string) (domain.AuditEntry, error) {
	var e domain.AuditEntry
	err := withPool(bindTenant(ctx, tenantID), r.pool, func(tx pgx.Tx) error {
		var err error
		e, err = scanEntry(tx.QueryRow(ctx, getEntrySQL, tenantID, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return port.ErrNotFound
		}
		return err
	})
	return e, err
}

const planWindowSQL = `SELECT plan_code, query_window_days FROM tenant_plan_window WHERE tenant_id = $1`

// PlanWindow implements port.AuditReader.
func (r *QueryRepository) PlanWindow(ctx context.Context, tenantID string) (*domain.PlanWindowRow, error) {
	var row *domain.PlanWindowRow
	err := withPool(bindTenant(ctx, tenantID), r.pool, func(tx pgx.Tx) error {
		var pw domain.PlanWindowRow
		err := tx.QueryRow(ctx, planWindowSQL, tenantID).Scan(&pw.PlanCode, &pw.QueryWindowDays)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return nil
		case err != nil:
			return err
		}
		row = &pw
		return nil
	})
	return row, err
}

// archivedObjectsSQL: sealed manifest rows overlapping the range. An object
// is sealed when its partition is dropped (000008), so its rows exist only
// in S3; unsealed objects' rows are still in RDS. Each row is therefore
// read from exactly one source, even for a re-opened month (D-12, D-19/D-20).
const archiveObjectColumns = `s3_bucket, s3_key, tenant_id::text, retention_tier::text, period_month, part,
    row_count, byte_size, min_occurred_at, max_occurred_at, min_id::text, max_id::text`

const archivedObjectsSQL = `SELECT ` + archiveObjectColumns + `
FROM audit_archive_objects
WHERE tenant_id = $1
  AND max_occurred_at >= $2 AND min_occurred_at <= $3
  AND ($4 = '' OR retention_tier::text = $4)
  AND sealed
ORDER BY max_occurred_at DESC, part`

// archivedObjectsForIDSQL: the sealed objects whose id range contains the id
// (AL-2 archived fallback, D-12).
const archivedObjectsForIDSQL = `SELECT ` + archiveObjectColumns + `
FROM audit_archive_objects
WHERE tenant_id = $1
  AND min_id <= $2::uuid AND max_id >= $2::uuid
  AND max_occurred_at >= $3
  AND sealed
ORDER BY max_occurred_at DESC, part`

// ArchivedObjects implements port.AuditReader.
func (r *QueryRepository) ArchivedObjects(ctx context.Context, tenantID string, from, to time.Time, tier string) ([]domain.ArchiveObject, error) {
	return r.archiveObjects(ctx, tenantID, archivedObjectsSQL, tenantID, from, to, tier)
}

// ArchivedObjectsForID implements port.AuditReader.
func (r *QueryRepository) ArchivedObjectsForID(ctx context.Context, tenantID, id string, from time.Time) ([]domain.ArchiveObject, error) {
	return r.archiveObjects(ctx, tenantID, archivedObjectsForIDSQL, tenantID, id, from)
}

func (r *QueryRepository) archiveObjects(ctx context.Context, tenantID, sql string, args ...any) ([]domain.ArchiveObject, error) {
	var out []domain.ArchiveObject
	err := withPool(bindTenant(ctx, tenantID), r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				o    domain.ArchiveObject
				tier string
			)
			if err := rows.Scan(&o.Bucket, &o.Key, &o.TenantID, &tier, &o.PeriodMonth, &o.Part,
				&o.RowCount, &o.ByteSize, &o.MinOccurredAt, &o.MaxOccurredAt, &o.MinID, &o.MaxID); err != nil {
				return err
			}
			o.Tier = domain.RetentionTier(tier)
			out = append(out, o)
		}
		return rows.Err()
	})
	return out, err
}
