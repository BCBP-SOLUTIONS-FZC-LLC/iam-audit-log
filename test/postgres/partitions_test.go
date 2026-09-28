//go:build integration

package postgres_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
)

// LLD §4.4: the bootstrap migration pre-creates the current month, 3 ahead
// and 3 trailing, each bounded by UTC month edges.
func TestPartitions_BootstrapWindowWithUTCBounds(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	ctx := context.Background()
	for off := -3; off <= 3; off++ {
		var bound string
		err := db.raw.QueryRow(ctx, `SELECT pg_get_expr(c.relpartbound, c.oid) FROM pg_class c WHERE c.relname = $1`,
			partitionName(off)).Scan(&bound)
		require.NoError(t, err, partitionName(off))
		assert.Contains(t, bound, monthStart(off).Format("2006-01-02 15:04:05")+"+00", partitionName(off))
		assert.Contains(t, bound, monthStart(off+1).Format("2006-01-02 15:04:05")+"+00", partitionName(off))
	}
}

// Rows route by occurred_at: into their monthly partition inside the
// window, into audit_events_default outside every explicit partition.
func TestPartitions_RoutingAndDefaultCatch(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	inWindow := newEvent(tenantA, monthStart(1).Add(36*time.Hour))
	seedEvent(t, db, inWindow)
	assert.Equal(t, partitionName(1), partitionOf(t, db, inWindow.SourceID))
	assert.Equal(t, domain.PartitionName(inWindow.OccurredAt), partitionOf(t, db, inWindow.SourceID))

	badClock := newEvent(tenantA, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
	seedEvent(t, db, badClock)
	assert.Equal(t, "audit_events_default", partitionOf(t, db, badClock.SourceID))
}

// PartitionService via each runtime role (both hold EXECUTE on the
// SECURITY DEFINER function): idempotent, extends the window on demand.
func TestPartitions_EnsureViaBothRoles(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	ctx := context.Background()

	res, err := pgadapter.NewPartitionRepository(db.appPool).EnsurePartitions(ctx, 3, 3)
	require.NoError(t, err)
	require.Len(t, res, 7)
	for _, r := range res {
		assert.Equal(t, domain.PartitionExists, r.Action, "bootstrap already created %s", r.Name)
	}

	res, err = pgadapter.NewPartitionRepository(db.reconPool).EnsurePartitions(ctx, 5, 3)
	require.NoError(t, err)
	created := 0
	for _, r := range res {
		if r.Action == domain.PartitionCreated {
			created++
			assert.True(t, r.RangeEnd.After(r.RangeStart))
		}
	}
	assert.Equal(t, 2, created, "months +4 and +5 are new")

	_, err = pgadapter.NewPartitionRepository(db.appPool).EnsurePartitions(ctx, 25, 0)
	require.Error(t, err, "window is bounded to 24 months per side")
}

// RB-3: a month whose range already has rows in the DEFAULT partition is
// reported skipped (not a startup failure); the rest of the window is created.
func TestPartitions_DefaultRowsBlockThatMonthOnly(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	ctx := context.Background()
	stray := newEvent(tenantA, monthStart(5).Add(time.Hour)) // outside the bootstrap window
	seedEvent(t, db, stray)
	require.Equal(t, "audit_events_default", partitionOf(t, db, stray.SourceID))

	res, err := pgadapter.NewPartitionRepository(db.reconPool).EnsurePartitions(ctx, 6, 0)
	require.NoError(t, err)
	actions := map[string]domain.PartitionAction{}
	for _, r := range res {
		actions[r.Name] = r.Action
	}
	assert.Equal(t, domain.PartitionSkippedDefaultHasRows, actions[partitionName(5)])
	assert.Equal(t, domain.PartitionCreated, actions[partitionName(4)])
	assert.Equal(t, domain.PartitionCreated, actions[partitionName(6)])
}

// Three replicas starting at once race to create the same months; every
// caller succeeds and each month exists exactly once.
func TestPartitions_ConcurrentEnsureIsSafe(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := pgadapter.NewPartitionRepository(db.appPool).EnsurePartitions(ctx, 8, 0)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		assert.NoError(t, err)
	}
	var n int
	require.NoError(t, db.raw.QueryRow(ctx, `SELECT count(*) FROM pg_class WHERE relname = $1`, partitionName(8)).Scan(&n))
	assert.Equal(t, 1, n)
}

// A partition created at runtime inherits isolation and immutability from
// the parent: RLS scopes it (AL-INV-3) and the append-only trigger guards
// it (AL-INV-1).
func TestPartitions_NewPartitionInheritsRLSAndTriggers_ALINV1_ALINV3(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	ctx := context.Background()
	_, err := pgadapter.NewPartitionRepository(db.reconPool).EnsurePartitions(ctx, 7, 0)
	require.NoError(t, err)

	e := newEvent(tenantA, monthStart(7).Add(time.Hour))
	seedEvent(t, db, e)
	require.Equal(t, partitionName(7), partitionOf(t, db, e.SourceID))

	assert.Equal(t, 0, countVisible(t, withTenant(ctx, tenantB), db.appPool))
	assert.Equal(t, 1, countVisible(t, withTenant(ctx, tenantA), db.appPool))

	_, err = db.raw.Exec(ctx, `UPDATE audit_events SET action = 'x' WHERE source_event_id = $1`, e.SourceID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "append-only (AL-INV-1)")

	err = inTx(withTenant(ctx, tenantA), db.appPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, insertEventSQL, newEvent(tenantA, monthStart(7).Add(2*time.Hour)).args()...)
		return err
	})
	require.NoError(t, err, "audit_app inserts route into the new partition through the parent")
}
