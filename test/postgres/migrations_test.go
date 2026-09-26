//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/adapter/outbound/postgres"
	pgmigrate "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/migrate"
)

// The embedded migration set applies cleanly and re-applies as a no-op
// (the server self-migrates on every start — LLD §4.4).
func TestMigrations_ApplyIdempotently(t *testing.T) {
	db := setupTestDB(t, dbOpts{}) // first apply happens in the harness
	require.NoError(t, pgadapter.RunMigrations(context.Background(), db.roles.SuperDSN, nil))
}

// AL-INV-10 / LLD §4.4: there is no outbox.ApplySchema step — no outbox
// tables may ever exist in the audit database.
func TestMigrations_CreateNoOutboxTables_ALINV10(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	for _, table := range []string{"outbox_events", "outbox_dead_letters"} {
		var exists bool
		require.NoError(t, db.raw.QueryRow(context.Background(),
			`SELECT to_regclass('public.' || $1) IS NOT NULL`, table).Scan(&exists))
		assert.False(t, exists, "%s must not exist (AL-INV-10)", table)
	}
}

func runner(db *testDB) *pgmigrate.Runner {
	return &pgmigrate.Runner{FS: pgadapter.MigrationsFS(), DSN: db.roles.SuperDSN}
}

func regclassExists(t *testing.T, db *testDB, name string) bool {
	t.Helper()
	var ok bool
	require.NoError(t, db.raw.QueryRow(context.Background(), `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&ok))
	return ok
}

// LLD §4.4 / §19: every migration is individually reversible — the full
// set rolls back one step at a time to an empty schema and re-applies.
func TestMigrations_FullDownUpRoundTrip(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	ctx := context.Background()
	r := runner(db)

	v, dirty, err := r.Version(ctx)
	require.NoError(t, err)
	require.False(t, dirty)
	require.EqualValues(t, 8, v)

	for step := 8; step >= 1; step-- {
		require.NoError(t, r.Down(ctx, 1), "down from version %d", step)
	}
	for _, obj := range []string{"audit_events", "audit_event_archive_state", "processed_events",
		"tenant_plan_window", "audit_export_jobs", "audit_redaction_tasks", "rls_violation_log", "audit_archive_objects"} {
		assert.False(t, regclassExists(t, db, obj), "%s must be gone after full down", obj)
	}

	require.NoError(t, r.Up(ctx))
	v, _, err = r.Version(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 8, v)
	assert.True(t, regclassExists(t, db, partitionName(0)), "bootstrap re-creates the partitions")
}

// Rolling back 000005 drops the function and EMPTY partitions only — a
// partition holding audit rows is never destroyed by a down step (AL-INV-1).
func TestMigrations_PartitionBootstrapDownKeepsData_ALINV1(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	ctx := context.Background()
	e := newEvent(tenantA, monthStart(0).Add(time.Hour))
	seedEvent(t, db, e)

	require.NoError(t, runner(db).Down(ctx, 4)) // 000008, 000007, 000006, then 000005
	assert.True(t, regclassExists(t, db, partitionName(0)), "non-empty partition must survive")
	assert.False(t, regclassExists(t, db, partitionName(1)), "empty partition is removed")

	var fn bool
	require.NoError(t, db.raw.QueryRow(ctx, `SELECT to_regprocedure('audit_ensure_partitions(int,int)') IS NOT NULL`).Scan(&fn))
	assert.False(t, fn)

	var n int
	require.NoError(t, db.raw.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE source_event_id = $1`, e.SourceID).Scan(&n))
	assert.Equal(t, 1, n)
	require.NoError(t, runner(db).Up(ctx), "re-apply after partial down")
}
