//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The four canonical fail-closed cases (LLD §14 "Postgres (RLS)") on the
// RLS-bound audit_app role, re-run against the real migrations.

// Case 1 — AL-INV-3: no GUC bound → zero rows, never all rows.
func TestRLS_Case1_MissingGUCReturnsZeroRows_ALINV3(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	seedEvent(t, db, newEvent(tenantA, time.Now()))
	seedEvent(t, db, newEvent(tenantB, time.Now()))
	assert.Equal(t, 0, countVisible(t, context.Background(), db.appPool))
}

// Case 2 — AL-INV-3 / §10.2: a cross-tenant INSERT is rejected by WITH CHECK
// (the structural guarantee against forged direct-write history).
func TestRLS_Case2_CrossTenantInsertRejected_ALINV3(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	ctx := withTenant(context.Background(), tenantA)

	err := inTx(ctx, db.appPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, insertEventSQL, newEvent(tenantB, time.Now()).args()...)
		return err
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "row-level security")

	// Same-tenant insert is accepted and visible to that tenant only.
	require.NoError(t, inTx(ctx, db.appPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, insertEventSQL, newEvent(tenantA, time.Now()).args()...)
		return err
	}))
	assert.Equal(t, 1, countVisible(t, ctx, db.appPool))
	assert.Equal(t, 0, countVisible(t, withTenant(context.Background(), tenantB), db.appPool))
}

// Case 3 — AL-INV-3: a malformed GUC → zero rows.
func TestRLS_Case3_MalformedGUCReturnsZeroRows_ALINV3(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	seedEvent(t, db, newEvent(tenantA, time.Now()))
	ctx := context.Background()
	var n int
	require.NoError(t, inTx(ctx, db.appPool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', 'not-a-uuid', true)`); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_events`).Scan(&n)
	}))
	assert.Zero(t, n)
}

// Tenant scoping: A sees exactly its own rows, B exactly its own.
func TestRLS_TenantSeesOnlyOwnRows_ALINV3(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	for range 3 {
		seedEvent(t, db, newEvent(tenantA, time.Now()))
	}
	seedEvent(t, db, newEvent(tenantB, time.Now()))
	assert.Equal(t, 3, countVisible(t, withTenant(context.Background(), tenantA), db.appPool))
	assert.Equal(t, 1, countVisible(t, withTenant(context.Background(), tenantB), db.appPool))
}

// AL-D14: platform_tenant rows are structurally invisible to any real tenant.
func TestRLS_PlatformTenantRowsInvisibleToTenants_ALD14(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	seedEvent(t, db, newEvent(platformTenant, time.Now()))
	assert.Equal(t, 0, countVisible(t, withTenant(context.Background(), tenantA), db.appPool))
	assert.Equal(t, 1, countVisible(t, withTenant(context.Background(), platformTenant), db.appPool),
		"the sentinel is an ordinary tenant_id to RLS")
}

// Case 4 — AL-INV-1: audit_app can neither UPDATE nor DELETE, even its own
// tenant's rows (permission denied by the grant set).
func TestRLS_Case4_AppRoleCannotUpdateOrDelete_ALINV1(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	e := newEvent(tenantA, time.Now())
	seedEvent(t, db, e)
	ctx := withTenant(context.Background(), tenantA)
	for _, stmt := range []string{
		`UPDATE audit_events SET action = 'tampered' WHERE source_event_id = $1`,
		`DELETE FROM audit_events WHERE source_event_id = $1`,
	} {
		err := inTx(ctx, db.appPool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, stmt, e.SourceID)
			return err
		})
		require.Error(t, err, stmt)
		assert.Contains(t, err.Error(), "permission denied", stmt)
	}
}

// AL-INV-1 defense-in-depth: the forbid_audit_mutation trigger refuses
// UPDATE/DELETE even from a privileged session (superuser/owner), naming
// the invariant.
func TestTrigger_PrivilegedSessionCannotMutate_ALINV1(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	e := newEvent(tenantA, time.Now())
	seedEvent(t, db, e)
	ctx := context.Background()
	for _, stmt := range []string{
		`UPDATE audit_events SET action = 'tampered' WHERE source_event_id = $1`,
		`DELETE FROM audit_events WHERE source_event_id = $1`,
	} {
		_, err := db.raw.Exec(ctx, stmt, e.SourceID)
		require.Error(t, err, stmt)
		assert.Contains(t, err.Error(), "append-only (AL-INV-1)", stmt)
	}
}

// §4.3 / §10.4: audit_reconciler is the sole DELETE path (trigger + grant
// allow it) and still has no UPDATE grant.
func TestReconciler_DeleteAllowedUpdateDenied(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	e := newEvent(tenantA, time.Now())
	seedEvent(t, db, e)
	ctx := context.Background()

	err := inTx(ctx, db.reconPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE audit_events SET action = 'x' WHERE source_event_id = $1`, e.SourceID)
		return err
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "permission denied")

	var n int64
	require.NoError(t, inTx(ctx, db.reconPool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM audit_events WHERE source_event_id = $1`, e.SourceID)
		n = tag.RowsAffected()
		return err
	}))
	assert.EqualValues(t, 1, n, "BYPASSRLS reconciler deletes across tenants")
}

// audit_export_jobs: RLS-scoped per tenant; audit_app may update its own
// tenant's job status (decision D-2) and touch_row bumps updated_at.
func TestRLS_ExportJobs_ALINV3(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	ctxA := withTenant(context.Background(), tenantA)
	ctxB := withTenant(context.Background(), tenantB)

	var id string
	var created time.Time
	require.NoError(t, inTx(ctxA, db.appPool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctxA, `INSERT INTO audit_export_jobs (tenant_id, requested_by, filter)
			VALUES ($1, $2, '{}'::jsonb) RETURNING id, updated_at`, tenantA, tenantA).Scan(&id, &created)
	}))

	err := inTx(ctxA, db.appPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctxA, `INSERT INTO audit_export_jobs (tenant_id, requested_by, filter) VALUES ($1, $1, '{}')`, tenantB)
		return err
	})
	require.Error(t, err, "cross-tenant export job must be rejected")

	var visibleToB int
	require.NoError(t, inTx(ctxB, db.appPool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctxB, `SELECT count(*) FROM audit_export_jobs`).Scan(&visibleToB)
	}))
	assert.Zero(t, visibleToB)

	time.Sleep(10 * time.Millisecond)
	var updated time.Time
	require.NoError(t, inTx(ctxA, db.appPool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctxA, `UPDATE audit_export_jobs SET status = 'running' WHERE id = $1 RETURNING updated_at`, id).Scan(&updated)
	}))
	assert.True(t, updated.After(created), "touch_row must bump updated_at")
}
