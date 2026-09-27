//go:build integration

package postgres_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
)

// Phase 8 (LLD §11, §20; decision D-21, gaps 40/41): audit_ops_stats()
// (migration 000009), read by cmd/server as audit_app through OpsRepository.

func opsQuery(hot, trailing int, grace, pending time.Duration) domain.OpsStatsQuery {
	return domain.OpsStatsQuery{HotWindowDays: hot, WritableTrailingMonths: trailing, StallGrace: grace, PendingAge: pending}
}

// attachedMonths lists the months of the attached monthly partitions.
func attachedMonths(t *testing.T, db *testDB) []time.Time {
	t.Helper()
	rows, err := db.raw.Query(context.Background(), `SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
WHERE i.inhparent = 'audit_events'::regclass AND c.relname ~ '^audit_events_[0-9]{4}_[0-9]{2}$'`)
	require.NoError(t, err)
	defer rows.Close()
	var out []time.Time
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		m, ok := domain.ParsePartitionName(name)
		require.True(t, ok, name)
		out = append(out, m)
	}
	require.NoError(t, rows.Err())
	return out
}

// DEFAULT rows are counted; the fresh bootstrap (current ±3 months) has no
// eligible partition, so stalled and lag are 0.
func TestOpsStats_DefaultRowsAndFreshDatabase(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	repo := pgadapter.NewOpsRepository(db.appPool)
	ctx := context.Background()

	s, err := repo.OpsStats(ctx, opsQuery(90, 3, 0, 15*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, domain.OpsStats{}, s, "fresh database: nothing to report")

	// Far-future rows route to audit_events_default.
	for range 2 {
		seedEvent(t, db, newEvent(tenantA, time.Date(2099, 1, 2, 0, 0, 0, 0, time.UTC)))
	}
	s, err = repo.OpsStats(ctx, opsQuery(90, 3, 0, 15*time.Minute))
	require.NoError(t, err)
	assert.EqualValues(t, 2, s.DefaultPartitionRows)
}

// An old partition (12 months back) past eligibility is stalled and has a
// positive lag; with a grace longer than its age it is not stalled, but
// the lag is still reported.
func TestOpsStats_StalledAndLag_ALINV9(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	repo := pgadapter.NewOpsRepository(db.appPool)
	ctx := context.Background()
	_, err := db.raw.Exec(ctx, `SELECT count(*) FROM audit_ensure_partitions(3, 12)`)
	require.NoError(t, err)

	s, err := repo.OpsStats(ctx, opsQuery(90, 3, 48*time.Hour, 15*time.Minute))
	require.NoError(t, err)
	assert.GreaterOrEqual(t, s.StalledPartitions, int64(1), "12- to 5-month-old partitions are past eligibility + 48 h")
	// The oldest (month -12) became eligible at greatest(end+90d, start+4 months),
	// i.e. roughly 8 months ago.
	oldest := monthStart(-12)
	eligibleAt := oldest.AddDate(0, 1, 90)
	if alt := oldest.AddDate(0, 4, 0); alt.After(eligibleAt) {
		eligibleAt = alt
	}
	assert.InDelta(t, time.Since(eligibleAt).Seconds(), s.ArchiveLagSeconds, 120, "lag = now − eligible_at of the oldest attached partition")

	s, err = repo.OpsStats(ctx, opsQuery(90, 3, 10*365*24*time.Hour, 15*time.Minute))
	require.NoError(t, err)
	assert.Zero(t, s.StalledPartitions, "a grace longer than any age → nothing stalled")
	assert.Greater(t, s.ArchiveLagSeconds, 0.0, "lag does not depend on the grace")
}

// Eligibility in SQL matches domain.ArchiveEligible: with grace 0, the
// stalled count equals the Go count over the attached partitions, for a
// spread of schedules.
func TestOpsStats_EligibilityMatchesDomain(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	repo := pgadapter.NewOpsRepository(db.appPool)
	ctx := context.Background()
	_, err := db.raw.Exec(ctx, `SELECT count(*) FROM audit_ensure_partitions(3, 14)`)
	require.NoError(t, err)
	months := attachedMonths(t, db)
	require.GreaterOrEqual(t, len(months), 18)

	for _, tc := range []struct{ hot, trailing int }{
		{1, 0}, {10, 0}, {30, 0}, {31, 1}, {45, 1}, {90, 3}, {10, 2}, {120, 0}, {200, 5}, {400, 0}, {1, 24},
	} {
		now := time.Now().UTC()
		var want int64
		for _, m := range months {
			if domain.ArchiveEligible(m, now, tc.hot, tc.trailing) {
				want++
			}
		}
		s, err := repo.OpsStats(ctx, opsQuery(tc.hot, tc.trailing, 0, time.Minute))
		require.NoError(t, err)
		assert.Equal(t, want, s.StalledPartitions, "hot=%d trailing=%d", tc.hot, tc.trailing)
		if want == 0 {
			assert.Zero(t, s.ArchiveLagSeconds, "hot=%d trailing=%d: no eligible partition → lag 0", tc.hot, tc.trailing)
		} else {
			assert.Greater(t, s.ArchiveLagSeconds, 0.0, "hot=%d trailing=%d", tc.hot, tc.trailing)
		}
	}
}

// Only pending tasks older than the age count; newer pending and finished
// tasks do not.
func TestOpsStats_PendingRedactions_RB7(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	repo := pgadapter.NewOpsRepository(db.appPool)
	ctx := context.Background()
	insert := func(status string, age time.Duration) {
		_, err := db.raw.Exec(ctx, `INSERT INTO audit_redaction_tasks
			(id, tenant_id, subject_actor_id, trigger_event_type, trigger_source_event_id, requested_at, status)
			VALUES ($1, $2, $3, 'UserDeleted', $4, now() - $5::interval, $6::audit_redaction_status)`,
			uuid.NewString(), tenantA, uuid.NewString(), uuid.NewString(), fmt.Sprintf("%d seconds", int(age.Seconds())), status)
		require.NoError(t, err)
	}
	insert("pending", time.Hour)
	insert("pending", 2*time.Hour)
	insert("pending", time.Minute) // recent: the immediate apply may still be running
	insert("applied", 3*time.Hour)
	insert("missed", 3*time.Hour)
	insert("not_applicable", 3*time.Hour)

	s, err := repo.OpsStats(ctx, opsQuery(90, 3, 0, 15*time.Minute))
	require.NoError(t, err)
	assert.EqualValues(t, 2, s.PendingRedactions)
	s, err = repo.OpsStats(ctx, opsQuery(90, 3, 0, 0))
	require.NoError(t, err)
	assert.EqualValues(t, 3, s.PendingRedactions, "age 0 counts every pending task")
}

// Bad parameters are rejected by the function.
func TestOpsStats_InvalidParameters(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	repo := pgadapter.NewOpsRepository(db.appPool)
	ctx := context.Background()
	for name, q := range map[string]domain.OpsStatsQuery{
		"hot 0":          opsQuery(0, 3, 0, 0),
		"trailing -1":    opsQuery(90, -1, 0, 0),
		"trailing 25":    opsQuery(90, 25, 0, 0),
		"negative grace": opsQuery(90, 3, -time.Hour, 0),
		"negative age":   opsQuery(90, 3, 0, -time.Minute),
	} {
		_, err := repo.OpsStats(ctx, q)
		assert.Error(t, err, name)
	}
}

// D-21 least privilege: audit_app may EXECUTE audit_ops_stats() but still
// cannot read the reconciler-only tables or name the DEFAULT partition.
// The function is SECURITY DEFINER and not executable by PUBLIC.
func TestOpsStats_GrantsAndDefiner(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	ctx := context.Background()
	_, err := db.raw.Exec(ctx, `CREATE ROLE ops_stranger NOLOGIN`)
	require.NoError(t, err)
	const fn = "audit_ops_stats(int,int,interval,interval)"

	var appOK, strangerOK, reconOK, definer bool
	require.NoError(t, db.raw.QueryRow(ctx, `SELECT has_function_privilege('audit_app', $1, 'EXECUTE'),
	        has_function_privilege('ops_stranger', $1, 'EXECUTE'),
	        has_function_privilege('audit_reconciler', $1, 'EXECUTE')`, fn).Scan(&appOK, &strangerOK, &reconOK))
	require.NoError(t, db.raw.QueryRow(ctx, `SELECT prosecdef FROM pg_proc WHERE oid = $1::regprocedure`, fn).Scan(&definer))
	assert.True(t, appOK, "audit_app executes audit_ops_stats")
	assert.False(t, strangerOK, "PUBLIC must not execute audit_ops_stats")
	assert.False(t, reconOK, "only audit_app needs it (cmd/server)")
	assert.True(t, definer, "SECURITY DEFINER")

	for _, sql := range []string{
		`SELECT count(*) FROM audit_event_archive_state`,
		`SELECT count(*) FROM audit_redaction_tasks`,
		`SELECT count(*) FROM audit_events_default`,
	} {
		err := inTx(withTenant(ctx, tenantA), db.appPool, func(tx pgx.Tx) error {
			var n int64
			return tx.QueryRow(ctx, sql).Scan(&n)
		})
		require.Error(t, err, sql)
		assert.Contains(t, err.Error(), "permission denied", sql)
	}
}
