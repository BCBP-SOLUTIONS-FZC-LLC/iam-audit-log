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

// OpsRepository implements port.OpsStatsReader on the audit_app pool via the
// audit_ops_stats() definer function (migration 000009, D-21).
type OpsRepository struct {
	pool *pgcommon.Pool
}

var _ port.OpsStatsReader = (*OpsRepository)(nil)

// NewOpsRepository returns a reader over the audit_app pool.
func NewOpsRepository(pool *pgcommon.Pool) *OpsRepository {
	return &OpsRepository{pool: pool}
}

// OpsStats implements port.OpsStatsReader.
func (r *OpsRepository) OpsStats(ctx context.Context, q domain.OpsStatsQuery) (domain.OpsStats, error) {
	var s domain.OpsStats
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT default_rows, stalled_partitions, archive_lag_seconds, pending_redactions
FROM audit_ops_stats($1, $2, $3::interval, $4::interval)`,
			q.HotWindowDays, q.WritableTrailingMonths, seconds(q.StallGrace), seconds(q.PendingAge)).
			Scan(&s.DefaultPartitionRows, &s.StalledPartitions, &s.ArchiveLagSeconds, &s.PendingRedactions)
	})
	return s, err
}

// seconds renders a duration as a Postgres interval literal.
func seconds(d time.Duration) string { return fmt.Sprintf("%d seconds", int64(d/time.Second)) }
