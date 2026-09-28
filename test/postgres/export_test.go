//go:build integration

package postgres_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
)

func exportFilter() domain.QueryFilter {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 3, 31, 23, 59, 59, 0, time.UTC)
	return domain.QueryFilter{From: &from, To: &to, EntryTypes: []string{"user.updated"}, RetentionTier: "security_3y"}
}

func newJob(tenant, requester string, f domain.QueryFilter) domain.ExportJob {
	return domain.ExportJob{ID: uuid.NewString(), TenantID: tenant, RequestedBy: requester, Filter: f}
}

func jobStatus(t *testing.T, db *testDB, id string) string {
	t.Helper()
	var s string
	require.NoError(t, db.raw.QueryRow(context.Background(), `SELECT status::text FROM audit_export_jobs WHERE id = $1`, id).Scan(&s))
	return s
}

// AL-3 persistence under audit_app: the row is pending and the jsonb filter
// round-trips (bound as a string — gap 28). Identical requests are distinct
// jobs (gap 31: no dedup).
func TestExport_CreateGet(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	repo := pgadapter.NewExportRepository(db.appPool)
	ctx := context.Background()
	me := uuid.NewString()
	f := exportFilter()

	job, err := repo.Create(ctx, newJob(tenantA, me, f))
	require.NoError(t, err)
	assert.Equal(t, domain.ExportPending, job.Status)
	assert.False(t, job.CreatedAt.IsZero())
	assert.Empty(t, job.S3Key)
	assert.Nil(t, job.RowCount)

	got, err := repo.Get(ctx, tenantA, job.ID)
	require.NoError(t, err)
	assert.Equal(t, job.ID, got.ID)
	assert.Equal(t, me, got.RequestedBy)
	require.NotNil(t, got.Filter.From)
	assert.True(t, f.From.Equal(*got.Filter.From))
	assert.Equal(t, f.EntryTypes, got.Filter.EntryTypes)
	assert.Equal(t, "security_3y", got.Filter.RetentionTier)

	_, err = repo.Get(ctx, tenantA, uuid.NewString())
	assert.ErrorIs(t, err, port.ErrNotFound)
	_, err = repo.Get(ctx, tenantB, job.ID)
	assert.ErrorIs(t, err, port.ErrNotFound, "RLS: another tenant's job is invisible")

	again, err := repo.Create(ctx, newJob(tenantA, me, f))
	require.NoError(t, err)
	assert.NotEqual(t, job.ID, again.ID)
}

// Decision D-2: audit_app, with no tenant GUC bound, claims the oldest
// pending job across tenants through claim_export_job(); it is returned
// with its tenant and filter and marked running.
func TestExport_ClaimOldestAcrossTenants_D2(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	repo := pgadapter.NewExportRepository(db.appPool)
	ctx := context.Background()

	first, err := repo.Create(ctx, newJob(tenantB, uuid.NewString(), exportFilter()))
	require.NoError(t, err)
	second, err := repo.Create(ctx, newJob(tenantA, uuid.NewString(), domain.QueryFilter{}))
	require.NoError(t, err)
	_, err = db.raw.Exec(ctx, `UPDATE audit_export_jobs SET created_at = now() - interval '1 minute' WHERE id = $1`, first.ID)
	require.NoError(t, err)

	c1, err := repo.Claim(ctx, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, c1)
	assert.Equal(t, first.ID, c1.ID, "oldest first, regardless of tenant")
	assert.Equal(t, tenantB, c1.TenantID)
	assert.Equal(t, first.RequestedBy, c1.RequestedBy)
	assert.Equal(t, domain.ExportRunning, c1.Status)
	assert.Equal(t, []string{"user.updated"}, c1.Filter.EntryTypes)
	assert.Equal(t, "running", jobStatus(t, db, first.ID))

	c2, err := repo.Claim(ctx, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, c2)
	assert.Equal(t, second.ID, c2.ID)

	c3, err := repo.Claim(ctx, time.Minute)
	require.NoError(t, err)
	assert.Nil(t, c3, "nothing left to claim")
}

// FOR UPDATE SKIP LOCKED: concurrent workers never claim the same job.
func TestExport_ConcurrentClaimsAreDistinct_D2(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	repo := pgadapter.NewExportRepository(db.appPool)
	ctx := context.Background()
	const n = 6
	for i := 0; i < n; i++ {
		_, err := repo.Create(ctx, newJob(tenantA, uuid.NewString(), domain.QueryFilter{}))
		require.NoError(t, err)
	}

	var (
		mu     sync.Mutex
		wg     sync.WaitGroup
		claims = map[string]int{}
	)
	for w := 0; w < n+2; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			j, err := repo.Claim(ctx, time.Minute)
			assert.NoError(t, err)
			if j != nil {
				mu.Lock()
				claims[j.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	for id, c := range claims {
		assert.Equal(t, 1, c, "job %s claimed %d times", id, c)
	}
	// Any job not claimed in the race (a worker that lost a lock race
	// returns nil) is still claimable afterwards.
	for len(claims) < n {
		j, err := repo.Claim(ctx, time.Minute)
		require.NoError(t, err)
		require.NotNil(t, j)
		_, dup := claims[j.ID]
		require.False(t, dup)
		claims[j.ID] = 1
	}
}

// A crashed worker's job: 'running' with a lapsed lease is re-claimed; a
// running job whose lease is fresh (or heartbeated) is not.
func TestExport_ExpiredLeaseReclaimed_D2(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	repo := pgadapter.NewExportRepository(db.appPool)
	ctx := context.Background()

	stale, err := repo.Create(ctx, newJob(tenantA, uuid.NewString(), domain.QueryFilter{}))
	require.NoError(t, err)
	fresh, err := repo.Create(ctx, newJob(tenantB, uuid.NewString(), domain.QueryFilter{}))
	require.NoError(t, err)
	for range 2 {
		j, err := repo.Claim(ctx, time.Minute)
		require.NoError(t, err)
		require.NotNil(t, j)
	}
	_, err = db.raw.Exec(ctx, `ALTER TABLE audit_export_jobs DISABLE TRIGGER audit_export_jobs_touch`)
	require.NoError(t, err)
	_, err = db.raw.Exec(ctx, `UPDATE audit_export_jobs SET updated_at = now() - interval '10 minutes', error = 'x' WHERE id = ANY($1::uuid[])`,
		[]string{stale.ID, fresh.ID})
	require.NoError(t, err)
	_, err = db.raw.Exec(ctx, `ALTER TABLE audit_export_jobs ENABLE TRIGGER audit_export_jobs_touch`)
	require.NoError(t, err)

	// The fresh job's worker is alive: its heartbeat renews the lease.
	require.NoError(t, repo.Heartbeat(ctx, tenantB, fresh.ID))

	j, err := repo.Claim(ctx, 5*time.Minute)
	require.NoError(t, err)
	require.NotNil(t, j)
	assert.Equal(t, stale.ID, j.ID)

	var errNull bool
	var age time.Duration
	require.NoError(t, db.raw.QueryRow(ctx, `SELECT error IS NULL, now() - updated_at FROM audit_export_jobs WHERE id = $1`, stale.ID).Scan(&errNull, &age))
	assert.True(t, errNull, "re-claim clears the stale error")
	assert.Less(t, age, time.Minute, "re-claim renews the lease")

	j, err = repo.Claim(ctx, 5*time.Minute)
	require.NoError(t, err)
	assert.Nil(t, j, "heartbeated job is not stolen")
}

// claim_export_job's lease is bounded to [1 minute, 1 day].
func TestExport_ClaimLeaseBounds(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	repo := pgadapter.NewExportRepository(db.appPool)
	ctx := context.Background()
	for _, lease := range []time.Duration{30 * time.Second, 25 * time.Hour} {
		_, err := repo.Claim(ctx, lease)
		require.Error(t, err, lease)
		assert.Contains(t, err.Error(), "lease must be between", lease)
	}
	for _, lease := range []time.Duration{time.Minute, 24 * time.Hour} {
		_, err := repo.Claim(ctx, lease)
		assert.NoError(t, err, lease)
	}
}

// Worker transitions fire only from the right state: Complete/Fail/Heartbeat
// require running; MarkExpired requires ready and a lapsed window.
func TestExport_StateTransitions(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	repo := pgadapter.NewExportRepository(db.appPool)
	ctx := context.Background()

	// Pending: no worker transition applies.
	p, err := repo.Create(ctx, newJob(tenantA, uuid.NewString(), domain.QueryFilter{}))
	require.NoError(t, err)
	require.NoError(t, repo.Complete(ctx, tenantA, p.ID, "k", 1, time.Now().Add(time.Hour)))
	require.NoError(t, repo.Fail(ctx, tenantA, p.ID, "internal_error"))
	require.NoError(t, repo.MarkExpired(ctx, tenantA, p.ID))
	assert.Equal(t, "pending", jobStatus(t, db, p.ID))

	// Running → ready.
	c, err := repo.Claim(ctx, time.Minute)
	require.NoError(t, err)
	require.Equal(t, p.ID, c.ID)
	exp := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	require.NoError(t, repo.Complete(ctx, tenantA, p.ID, "exports/a/b.jsonl.gz", 42, exp))
	ready, err := repo.Get(ctx, tenantA, p.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.ExportReady, ready.Status)
	assert.Equal(t, "exports/a/b.jsonl.gz", ready.S3Key)
	require.NotNil(t, ready.RowCount)
	assert.Equal(t, int64(42), *ready.RowCount)
	require.NotNil(t, ready.SignedURLExpiresAt)
	assert.True(t, exp.Equal(*ready.SignedURLExpiresAt))
	assert.NotNil(t, ready.CompletedAt)

	// Ready: Fail / Complete / Heartbeat are no-ops; MarkExpired waits for the window.
	require.NoError(t, repo.Fail(ctx, tenantA, p.ID, "internal_error"))
	require.NoError(t, repo.Complete(ctx, tenantA, p.ID, "other", 1, exp))
	require.NoError(t, repo.MarkExpired(ctx, tenantA, p.ID))
	again, err := repo.Get(ctx, tenantA, p.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.ExportReady, again.Status, "window not lapsed yet")
	assert.Equal(t, "exports/a/b.jsonl.gz", again.S3Key)

	_, err = db.raw.Exec(ctx, `UPDATE audit_export_jobs SET signed_url_expires_at = now() - interval '1 second' WHERE id = $1`, p.ID)
	require.NoError(t, err)
	require.NoError(t, repo.MarkExpired(ctx, tenantB, p.ID), "RLS: another tenant cannot expire it")
	assert.Equal(t, "ready", jobStatus(t, db, p.ID))
	require.NoError(t, repo.MarkExpired(ctx, tenantA, p.ID))
	assert.Equal(t, "expired", jobStatus(t, db, p.ID))

	// Running → failed, with the §17 reason recorded.
	f, err := repo.Create(ctx, newJob(tenantA, uuid.NewString(), domain.QueryFilter{}))
	require.NoError(t, err)
	_, err = repo.Claim(ctx, time.Minute)
	require.NoError(t, err)
	require.NoError(t, repo.Fail(ctx, tenantA, f.ID, "dependency_unavailable"))
	failed, err := repo.Get(ctx, tenantA, f.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.ExportFailed, failed.Status)
	assert.Equal(t, "dependency_unavailable", failed.Error)
	assert.NotNil(t, failed.CompletedAt)
	require.NoError(t, repo.Complete(ctx, tenantA, f.ID, "k", 1, exp))
	assert.Equal(t, "failed", jobStatus(t, db, f.ID), "a failed job never becomes ready")
}

// D-2 grants: claim_export_job is executable by audit_app only (not
// PUBLIC), and its owner bypasses RLS — otherwise FORCE RLS on
// audit_export_jobs would hide every job from the definer and the worker
// would silently never claim anything.
func TestExport_ClaimFunctionGrantsAndOwner_D2(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	ctx := context.Background()
	_, err := db.raw.Exec(ctx, `CREATE ROLE stranger NOLOGIN`)
	require.NoError(t, err)
	check := func(role string) bool {
		var ok bool
		require.NoError(t, db.raw.QueryRow(ctx, `SELECT has_function_privilege($1, 'claim_export_job(interval)', 'EXECUTE')`, role).Scan(&ok))
		return ok
	}
	assert.True(t, check("audit_app"))
	assert.False(t, check("stranger"), "PUBLIC must not execute claim_export_job")
	assert.False(t, check("audit_reconciler"))

	var definer, bypass bool
	require.NoError(t, db.raw.QueryRow(ctx, `SELECT p.prosecdef, r.rolsuper OR r.rolbypassrls
		FROM pg_proc p JOIN pg_roles r ON r.oid = p.proowner WHERE p.proname = 'claim_export_job'`).Scan(&definer, &bypass))
	assert.True(t, definer, "SECURITY DEFINER")
	assert.True(t, bypass, "owner must bypass RLS to see jobs across tenants")
}
