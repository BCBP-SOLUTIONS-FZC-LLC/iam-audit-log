package postgres

import (
	"context"
	"fmt"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/jackc/pgx/v5"
)

// RoleInfo is the connected session role and its RLS attribute.
type RoleInfo struct {
	Name      string
	BypassRLS bool
}

// CurrentRole reports which role pool's connections authenticate as.
func CurrentRole(ctx context.Context, pool *pgcommon.Pool) (RoleInfo, error) {
	var ri RoleInfo
	err := withPool(ctx, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT current_user::text, rolbypassrls FROM pg_roles WHERE rolname = current_user`,
		).Scan(&ri.Name, &ri.BypassRLS)
	})
	return ri, err
}

// CheckAppRole verifies cmd/server's pool connects as want and without
// BYPASSRLS — the app role must stay RLS-bound (LLD §4.3, AL-INV-3).
func CheckAppRole(ri RoleInfo, want string) error {
	if ri.Name != want {
		return fmt.Errorf("app pool connected as %q, expected %q (DB_APP_ROLE)", ri.Name, want)
	}
	if ri.BypassRLS {
		return fmt.Errorf("app role %q has BYPASSRLS — audit_app must be RLS-bound (AL-INV-3)", ri.Name)
	}
	return nil
}

// CheckReconcilerRole verifies cmd/reconciler's pool connects as want
// (audit_reconciler, BYPASSRLS — LLD §4.3, §10.4).
func CheckReconcilerRole(ri RoleInfo, want string) error {
	if ri.Name != want {
		return fmt.Errorf("reconciler pool connected as %q, expected %q (DB_RECONCILER_ROLE)", ri.Name, want)
	}
	if !ri.BypassRLS {
		return fmt.Errorf("reconciler role %q lacks BYPASSRLS — cross-tenant archival would see zero rows", ri.Name)
	}
	return nil
}
