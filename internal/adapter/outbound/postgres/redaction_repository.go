package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
)

// RedactionRepository implements port.RedactionTasks on the
// audit_reconciler pool (SELECT/UPDATE on audit_redaction_tasks; the task
// table is RLS-exempt, LLD §4.2).
type RedactionRepository struct {
	pool *pgcommon.Pool
}

var _ port.RedactionTasks = (*RedactionRepository)(nil)

// NewRedactionRepository returns a repository over the reconciler pool.
func NewRedactionRepository(pool *pgcommon.Pool) *RedactionRepository {
	return &RedactionRepository{pool: pool}
}

const pendingTasksSQL = `SELECT id::text FROM audit_redaction_tasks
WHERE status = 'pending' AND requested_at < $1
ORDER BY requested_at
LIMIT $2`

// PendingTasks implements port.RedactionTasks.
func (r *RedactionRepository) PendingTasks(ctx context.Context, olderThan time.Time, limit int) ([]string, error) {
	var ids []string
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, pendingTasksSQL, olderThan, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	return ids, err
}

// ApplyRedaction implements port.RedactionTasks (apply_redaction(), D-1).
func (r *RedactionRepository) ApplyRedaction(ctx context.Context, taskID string) (domain.RedactionOutcome, error) {
	return applyRedaction(ctx, r.pool, taskID)
}

const sweepSQL = `SELECT sweep_redactions($1::interval)`

// Sweep implements port.RedactionTasks (decision D-18 defense in depth).
func (r *RedactionRepository) Sweep(ctx context.Context, window time.Duration) (int64, error) {
	var n int64
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, sweepSQL, fmt.Sprintf("%d seconds", int64(window/time.Second))).Scan(&n)
	})
	return n, err
}
