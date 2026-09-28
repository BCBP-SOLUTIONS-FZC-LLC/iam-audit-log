//go:build integration

package postgres_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/adapter/outbound/postgres"
)

// Rule 5 / LLD §4.3: each composition root authenticates as exactly its own
// role, and the startup role checks accept the right role and refuse the
// other one.
func TestRoles_EachPoolAuthenticatesAsItsOwnRole(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	ctx := context.Background()

	app, err := pgadapter.CurrentRole(ctx, db.appPool)
	require.NoError(t, err)
	assert.Equal(t, "audit_app", app.Name)
	assert.False(t, app.BypassRLS, "audit_app must be RLS-bound (AL-INV-3)")
	assert.NoError(t, pgadapter.CheckAppRole(app, "audit_app"))
	assert.Error(t, pgadapter.CheckReconcilerRole(app, "audit_reconciler"),
		"the app role must never pass as the reconciler")

	recon, err := pgadapter.CurrentRole(ctx, db.reconPool)
	require.NoError(t, err)
	assert.Equal(t, "audit_reconciler", recon.Name)
	assert.True(t, recon.BypassRLS, "audit_reconciler archives cross-tenant (§10.4)")
	assert.NoError(t, pgadapter.CheckReconcilerRole(recon, "audit_reconciler"))
	assert.Error(t, pgadapter.CheckAppRole(recon, "audit_app"),
		"the reconciler role must never pass as the app role")
}

// The app role acquiring BYPASSRLS (e.g. a mis-granted role) is refused at
// startup rather than silently disabling tenant isolation.
func TestRoles_AppRoleWithBypassRLSIsRefused_ALINV3(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	ctx := context.Background()
	_, err := db.raw.Exec(ctx, `ALTER ROLE audit_app BYPASSRLS`)
	require.NoError(t, err)

	cfg, _ := pgadapter.AppPoolConfig(db.roles.AppDSN, nil, nil)
	fresh := newPoolWithRetry(t, cfg)
	ri, err := pgadapter.CurrentRole(ctx, fresh)
	require.NoError(t, err)
	assert.Error(t, pgadapter.CheckAppRole(ri, "audit_app"))
}
