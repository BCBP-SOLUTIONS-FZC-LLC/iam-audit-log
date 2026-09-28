//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
)

const (
	tenantA = "11111111-1111-1111-1111-111111111111"
	tenantB = "22222222-2222-2222-2222-222222222222"
)

// queryInTx scans one row using the transaction RunInTx put on ctx.
func queryInTx(ctx context.Context, sql string, dest ...any) error {
	tx, ok := pgadapter.TxFromContext(ctx)
	if !ok {
		return errors.New("no transaction on context")
	}
	return tx.QueryRow(ctx, sql).Scan(dest...)
}

// currentTenantGUC reads app.tenant_id inside one RunInTx on pool.
func currentTenantGUC(t *testing.T, ctx context.Context, pool *pgcommon.Pool) string {
	t.Helper()
	var v string
	require.NoError(t, pgadapter.NewTxRunner(pool).RunInTx(ctx, func(ctx context.Context) error {
		return queryInTx(ctx, `SELECT coalesce(current_setting('app.tenant_id', true), '')`, &v)
	}))
	return v
}

// AL-INV-3 / RLS-6 (iam-org-membership Case 5): the tenant GUC is bound
// transaction-locally on every checkout and never leaks to the next
// transaction on the same pooled backend — the PgBouncer transaction-
// pooling hazard a session-level SET would create.
func TestGUC_TransactionLocalNoLeakAcrossPooledBackend_ALINV3(t *testing.T) {
	db := setupTestDB(t, dbOpts{appMaxConns: 1}) // one backend → guaranteed reuse
	ctx := context.Background()

	assert.Equal(t, tenantA, currentTenantGUC(t, withTenant(ctx, tenantA), db.appPool))
	assert.Equal(t, "", currentTenantGUC(t, ctx, db.appPool),
		"tenant A's GUC leaked into an unbound transaction on the same backend")
	assert.Equal(t, tenantB, currentTenantGUC(t, withTenant(ctx, tenantB), db.appPool))
	assert.Equal(t, tenantA, currentTenantGUC(t, withTenant(ctx, tenantA), db.appPool),
		"tenant B's GUC leaked into tenant A's transaction")
}

// The GUC is gone after COMMIT even within one session (is_local = true).
func TestGUC_ClearedAtCommit_ALINV3(t *testing.T) {
	db := setupTestDB(t, dbOpts{appMaxConns: 1})
	ctx := context.Background()

	var inTx, afterTx string
	require.NoError(t, pgadapter.NewTxRunner(db.appPool).RunInTx(withTenant(ctx, tenantA), func(ctx context.Context) error {
		return queryInTx(ctx, `SELECT current_setting('app.tenant_id', true)`, &inTx)
	}))
	require.NoError(t, db.raw.QueryRow(ctx, `SELECT 1`).Scan(new(int)))
	afterTx = currentTenantGUC(t, ctx, db.appPool)
	assert.Equal(t, tenantA, inTx)
	assert.Empty(t, afterTx)
}

// The reconciler pool binds no tenant even if the context carries one: it
// runs BYPASSRLS cross-tenant and must not be scoped by a stray GUC
// (LLD §3.3.2).
func TestGUC_ReconcilerPoolBindsNoTenant(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	assert.Equal(t, "", currentTenantGUC(t, withTenant(context.Background(), tenantA), db.reconPool))
}
