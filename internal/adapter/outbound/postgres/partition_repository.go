package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
)

// PartitionRepository implements port.PartitionManager through the
// audit_ensure_partitions() SECURITY DEFINER function (migration 000005),
// which both runtime roles may EXECUTE.
type PartitionRepository struct {
	pool *pgcommon.Pool
}

var _ port.PartitionManager = (*PartitionRepository)(nil)

// NewPartitionRepository returns a repository over pool.
func NewPartitionRepository(pool *pgcommon.Pool) *PartitionRepository {
	return &PartitionRepository{pool: pool}
}

// EnsurePartitions creates any missing monthly partition in the window.
func (r *PartitionRepository) EnsurePartitions(ctx context.Context, ahead, trailing int) ([]domain.PartitionResult, error) {
	var out []domain.PartitionResult
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT partition_name, range_start, range_end, action FROM audit_ensure_partitions($1, $2)`,
			ahead, trailing)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				res    domain.PartitionResult
				action string
			)
			if err := rows.Scan(&res.Name, &res.RangeStart, &res.RangeEnd, &action); err != nil {
				return err
			}
			res.Action = domain.PartitionAction(action)
			out = append(out, res)
		}
		return rows.Err()
	})
	return out, err
}
