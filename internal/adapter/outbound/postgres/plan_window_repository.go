package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
)

// PlanWindowRepository implements port.PlanProjector on tenant_plan_window
// (RLS-exempt, §4.2; audit_app holds SELECT/INSERT/UPDATE).
type PlanWindowRepository struct {
	pool *pgcommon.Pool
}

var _ port.PlanProjector = (*PlanWindowRepository)(nil)

// NewPlanWindowRepository returns a repository over the audit_app pool.
func NewPlanWindowRepository(pool *pgcommon.Pool) *PlanWindowRepository {
	return &PlanWindowRepository{pool: pool}
}

// Last-writer-wins on the driving event's time (O&M-style recency guard):
// an event no newer than the stored one changes nothing.
const upsertPlanWindowSQL = `INSERT INTO tenant_plan_window (tenant_id, plan_code, query_window_days, last_event_at, updated_at)
VALUES ($1, $2, $3, $4, now())
ON CONFLICT (tenant_id) DO UPDATE
   SET plan_code = EXCLUDED.plan_code,
       query_window_days = EXCLUDED.query_window_days,
       last_event_at = EXCLUDED.last_event_at,
       updated_at = now()
 WHERE tenant_plan_window.last_event_at < EXCLUDED.last_event_at`

// ProjectPlan implements port.PlanProjector.
func (r *PlanWindowRepository) ProjectPlan(ctx context.Context, tenantID, planCode string, days int, eventAt time.Time) (bool, error) {
	var applied bool
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, upsertPlanWindowSQL, tenantID, planCode, days, eventAt)
		applied = tag.RowsAffected() == 1
		return err
	})
	return applied, err
}
