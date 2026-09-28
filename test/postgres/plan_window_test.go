//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/adapter/outbound/postgres"
)

// §4.2 / AL-EVT-3: tenant_plan_window is last-writer-wins on the DRIVING
// EVENT's time — an out-of-order or replayed older event never regresses
// the plan; audit_app can maintain it (RLS-exempt).
func TestPlanWindow_RecencyGuard_ALEVT3(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	repo := pgadapter.NewPlanWindowRepository(db.appPool)
	ctx := context.Background()
	t1 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	applied, err := repo.ProjectPlan(ctx, tenantA, "starter", 365, t1)
	require.NoError(t, err)
	assert.True(t, applied, "first event creates the row")

	applied, err = repo.ProjectPlan(ctx, tenantA, "enterprise", 2555, t1.Add(time.Hour))
	require.NoError(t, err)
	assert.True(t, applied, "newer event wins")

	for _, stale := range []time.Time{t1, t1.Add(time.Hour)} { // older, and equal (a redelivery)
		applied, err = repo.ProjectPlan(ctx, tenantA, "starter", 365, stale)
		require.NoError(t, err)
		assert.False(t, applied, "stale/equal event must not regress the plan")
	}

	var plan string
	var days int
	require.NoError(t, db.raw.QueryRow(ctx, `SELECT plan_code, query_window_days FROM tenant_plan_window WHERE tenant_id = $1`, tenantA).Scan(&plan, &days))
	assert.Equal(t, "enterprise", plan)
	assert.Equal(t, 2555, days)
}
