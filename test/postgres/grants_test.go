//go:build integration

package postgres_test

import (
	"context"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func tableGrants(t *testing.T, db *testDB, role, table string) []string {
	t.Helper()
	rows, err := db.raw.Query(context.Background(), `
		SELECT privilege_type FROM information_schema.role_table_grants
		WHERE grantee = $1 AND table_schema = 'public' AND table_name = $2`, role, table)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		require.NoError(t, rows.Scan(&p))
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// AL-INV-1 (live half of check-grants.sh): the exact least-privilege grant
// matrix of LLD §4.3 / §10.4 plus decisions D-1/D-2.
func TestGrants_LeastPrivilegeMatrix_ALINV1(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	want := map[string]map[string][]string{
		"audit_app": {
			"audit_events":              {"INSERT", "SELECT"},
			"processed_events":          {"INSERT", "SELECT"},
			"tenant_plan_window":        {"INSERT", "SELECT", "UPDATE"},
			"audit_export_jobs":         {"INSERT", "SELECT", "UPDATE"},
			"audit_redaction_tasks":     {"INSERT"},
			"audit_event_archive_state": nil,
			"rls_violation_log":         nil,
		},
		"audit_reconciler": {
			"audit_events":              {"DELETE", "SELECT"},
			"audit_event_archive_state": {"INSERT", "SELECT", "UPDATE"},
			"processed_events":          {"DELETE", "SELECT"},
			"audit_redaction_tasks":     {"SELECT", "UPDATE"},
			"tenant_plan_window":        nil,
			"audit_export_jobs":         nil,
		},
	}
	for role, tables := range want {
		for table, privs := range tables {
			assert.Equal(t, privs, tableGrants(t, db, role, table), "%s on %s", role, table)
		}
	}
}

// §4.3: audit_app holds no privilege on any partition directly — the only
// way in is through the RLS-protected parent, so new monthly partitions
// "inherit" isolation automatically (MIG-4 equivalent).
func TestGrants_AppRoleCannotTouchPartitionsDirectly_ALINV3(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	ctx := context.Background()
	for _, p := range []string{partitionName(0), "audit_events_default"} {
		var has bool
		require.NoError(t, db.raw.QueryRow(ctx,
			`SELECT has_table_privilege('audit_app', $1, 'SELECT')`, p).Scan(&has))
		assert.False(t, has, p)

		err := inTx(withTenant(ctx, tenantA), db.appPool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SELECT 1 FROM `+p+` LIMIT 1`)
			return err
		})
		require.Error(t, err, p)
		assert.Contains(t, err.Error(), "permission denied", p)
	}
}

// SECURITY DEFINER functions are not callable by PUBLIC; each runtime role
// gets exactly what it needs.
func TestGrants_FunctionExecute(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	ctx := context.Background()
	_, err := db.raw.Exec(ctx, `CREATE ROLE stranger NOLOGIN`)
	require.NoError(t, err)

	check := func(role, fn string) bool {
		var ok bool
		require.NoError(t, db.raw.QueryRow(ctx, `SELECT has_function_privilege($1, $2, 'EXECUTE')`, role, fn).Scan(&ok))
		return ok
	}
	for _, fn := range []string{"audit_ensure_partitions(int,int)", "rls_check_tenant(uuid,text)", "log_rls_violation(text,uuid,text)", "app_tenant_id()"} {
		assert.False(t, check("stranger", fn), "PUBLIC must not execute %s", fn)
	}
	assert.True(t, check("audit_app", "audit_ensure_partitions(int,int)"))
	assert.True(t, check("audit_reconciler", "audit_ensure_partitions(int,int)"))
	assert.True(t, check("audit_app", "rls_check_tenant(uuid,text)"))
}

// Roles carry the RLS attributes §4.3 requires; admin_readonly is created
// NOLOGIN BYPASSRLS with SELECT only.
func TestRoles_Attributes(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	rows, err := db.raw.Query(context.Background(),
		`SELECT rolname, rolbypassrls FROM pg_roles WHERE rolname IN ('audit_app','audit_reconciler','audit_migrator','admin_readonly')`)
	require.NoError(t, err)
	defer rows.Close()
	got := map[string]bool{}
	for rows.Next() {
		var n string
		var b bool
		require.NoError(t, rows.Scan(&n, &b))
		got[n] = b
	}
	assert.Equal(t, map[string]bool{"audit_app": false, "audit_reconciler": true, "audit_migrator": true, "admin_readonly": true}, got)
	assert.Equal(t, []string{"SELECT"}, tableGrants(t, db, "admin_readonly", "audit_events"))
}
