package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
	"github.com/jackc/pgx/v5"
)

// JobLease keeps one reconciler run per job at a time (migration 000011).
// concurrencyPolicy: Forbid does not cover a manually created Job, and a
// session advisory lock does not survive PgBouncer transaction mode, so the
// run claims a lease row that expires on its own if the run dies.
type JobLease struct {
	pool *pgcommon.Pool
}

// NewJobLease constructs a JobLease over the reconciler pool.
func NewJobLease(pool *pgcommon.Pool) *JobLease { return &JobLease{pool: pool} }

const acquireLeaseSQL = `INSERT INTO reconciler_job_lease (job, holder, expires_at)
VALUES ($1, $2, now() + make_interval(secs => $3))
ON CONFLICT (job) DO UPDATE SET holder = EXCLUDED.holder, expires_at = EXCLUDED.expires_at
WHERE reconciler_job_lease.expires_at < now()
RETURNING holder`

// Claim claims job for holder until ttl from now. It returns false, with
// no error, when another holder's lease has not expired yet.
func (l *JobLease) Claim(ctx context.Context, job, holder string, ttl time.Duration) (bool, error) {
	err := withPool(ctx, l.pool, func(tx pgx.Tx) error {
		var got string
		return tx.QueryRow(ctx, acquireLeaseSQL, job, holder, ttl.Seconds()).Scan(&got)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Release drops holder's lease on job; a lease another holder took over
// after expiry is left alone.
func (l *JobLease) Release(ctx context.Context, job, holder string) error {
	return withPool(ctx, l.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM reconciler_job_lease WHERE job = $1 AND holder = $2`, job, holder)
		return err
	})
}
