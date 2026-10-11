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

// Migration 000011: one reconciler run per job at a time, manual Jobs
// included. A held lease refuses a second holder; an expired one is taken
// over; release only drops the caller's own lease; audit_app has no access.
func TestJobLease_OneRunPerJob(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	ctx := context.Background()
	lease := pgadapter.NewJobLease(db.reconPool)

	ok, err := lease.Claim(ctx, "reconcile", "run-a", time.Minute)
	require.NoError(t, err)
	assert.True(t, ok, "a free job is claimed")

	ok, err = lease.Claim(ctx, "reconcile", "run-b", time.Minute)
	require.NoError(t, err)
	assert.False(t, ok, "a held lease refuses a second run")

	ok, err = lease.Claim(ctx, "redaction-retry", "run-b", time.Minute)
	require.NoError(t, err)
	assert.True(t, ok, "leases are per job")

	require.NoError(t, lease.Release(ctx, "reconcile", "run-b"), "a non-holder's release is a no-op")
	ok, err = lease.Claim(ctx, "reconcile", "run-c", time.Minute)
	require.NoError(t, err)
	assert.False(t, ok, "run-a still holds it")

	require.NoError(t, lease.Release(ctx, "reconcile", "run-a"))
	ok, err = lease.Claim(ctx, "reconcile", "run-c", time.Millisecond)
	require.NoError(t, err)
	assert.True(t, ok, "released lease is free again")

	time.Sleep(20 * time.Millisecond)
	ok, err = lease.Claim(ctx, "reconcile", "run-d", time.Minute)
	require.NoError(t, err)
	assert.True(t, ok, "an expired lease (crashed run) is taken over")

	_, err = pgadapter.NewJobLease(db.appPool).Claim(ctx, "reconcile", "app", time.Minute)
	assert.Error(t, err, "audit_app must not touch the reconciler lease")
}
