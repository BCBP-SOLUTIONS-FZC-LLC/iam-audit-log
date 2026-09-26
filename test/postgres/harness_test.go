//go:build integration

// Package postgres_test is the Postgres-tier suite (LLD §14 "Postgres (RLS)"
// / "Postgres (partition)" rows), mirroring iam-org-membership/test/postgres:
// a fresh PostgreSQL 15 testcontainer per test, every embedded migration
// applied as superuser, and one pgcommon pool per LLD §4.3 runtime role:
//
//	appPool   — audit_app (NOBYPASSRLS), GUCProvider bound: every checkout
//	            issues set_config('app.tenant_id', …, true) exactly as
//	            cmd/server does in production (AL-INV-3).
//	reconPool — audit_reconciler (BYPASSRLS), no GUCProvider, PgBouncer
//	            mode — exactly as cmd/reconciler builds it.
//	raw       — dbseed superuser pool for seeding/asserting.
//
// Phase 0 has no schema yet; this tier covers the role/GUC mechanics the
// schema will rely on. Phase 1 adds the canonical RLS fail-closed Cases 1–4
// and the partition suite on top of this same harness.
//
// Requires Docker. Tag: integration.
package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/test/dbseed"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/test/fixtures"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
)

type testDB struct {
	roles     fixtures.Roles
	appPool   *pgcommon.Pool
	reconPool *pgcommon.Pool
	raw       *dbseed.Pool
}

type dbOpts struct {
	// appMaxConns pins the app pool size; 1 forces every transaction onto
	// the same backend (the PgBouncer-reuse scenario GUC leakage tests need).
	appMaxConns int32
}

func setupTestDB(t *testing.T, opts dbOpts) *testDB {
	t.Helper()
	ctx := context.Background()
	superDSN := fixtures.StartPostgres(t)
	roles := fixtures.CreateRoles(t, superDSN)

	require.NoError(t, pgadapter.RunMigrations(ctx, superDSN, nil), "apply embedded migrations")

	raw, err := dbseed.New(ctx, superDSN)
	require.NoError(t, err)
	t.Cleanup(raw.Close)

	appCfg, _ := pgadapter.AppPoolConfig(roles.AppDSN, nil, nil)
	if opts.appMaxConns > 0 {
		appCfg.MaxConns = opts.appMaxConns
		appCfg.MinConns = opts.appMaxConns // pgcommon replaces 0 with its default (2)
	}
	appPool := newPoolWithRetry(t, appCfg)

	reconCfg, _ := pgadapter.ReconcilerPoolConfig(roles.ReconcilerDSN, nil, nil)
	reconPool := newPoolWithRetry(t, reconCfg)

	return &testDB{roles: roles, appPool: appPool, reconPool: reconPool, raw: raw}
}

// newPoolWithRetry absorbs the brief window after container start where a
// freshly created role's first connection can race (same retry iam-org-
// membership's harness uses).
func newPoolWithRetry(t *testing.T, cfg pgcommon.Config) *pgcommon.Pool {
	t.Helper()
	var (
		pool *pgcommon.Pool
		err  error
	)
	for attempt := 1; attempt <= 3; attempt++ {
		pool, err = pgcommon.NewPool(context.Background(), cfg)
		if err == nil {
			t.Cleanup(pool.Close)
			return pool
		}
		time.Sleep(time.Duration(attempt) * 300 * time.Millisecond)
	}
	require.NoError(t, err)
	return nil
}

// withTenant binds tenantID as the RLS GUC for subsequent checkouts.
func withTenant(ctx context.Context, tenantID string) context.Context {
	g, _ := pgcommon.GUCSetFromContext(ctx)
	g.TenantID = tenantID
	return pgcommon.WithGUCSet(ctx, g)
}
