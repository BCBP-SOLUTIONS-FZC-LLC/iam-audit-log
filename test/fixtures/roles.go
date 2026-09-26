//go:build integration || e2e

package fixtures

import (
	"context"
	"fmt"
	"net/url"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/test/dbseed"
)

// Test-only role passwords.
const (
	AppRolePassword        = "apppassword-testonly"
	ReconcilerRolePassword = "reconcilerpassword-testonly"
)

// Roles holds the DSNs for each LLD §4.3 runtime role against one database.
type Roles struct {
	SuperDSN      string // postgres superuser (seed / migrator stand-in)
	AppDSN        string // audit_app — NOBYPASSRLS (cmd/server)
	ReconcilerDSN string // audit_reconciler — BYPASSRLS (cmd/reconciler)
}

// CreateRoles provisions audit_app (NOBYPASSRLS) and audit_reconciler
// (BYPASSRLS) the way LLD §4.3 / §10.4 specify, idempotently (the Phase 1
// roles migration will create them too), and returns a DSN per role.
func CreateRoles(t testing.TB, superDSN string) Roles {
	t.Helper()
	ctx := context.Background()
	seed, err := dbseed.New(ctx, superDSN)
	if err != nil {
		t.Fatalf("seed pool: %v", err)
	}
	defer seed.Close()

	for _, stmt := range []string{
		fmt.Sprintf(`DO $$ BEGIN
		  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'audit_app') THEN
		    CREATE ROLE audit_app LOGIN PASSWORD '%s' NOBYPASSRLS;
		  END IF;
		END $$`, AppRolePassword),
		fmt.Sprintf(`DO $$ BEGIN
		  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'audit_reconciler') THEN
		    CREATE ROLE audit_reconciler LOGIN PASSWORD '%s' BYPASSRLS;
		  END IF;
		END $$`, ReconcilerRolePassword),
		`GRANT CONNECT ON DATABASE audit TO audit_app, audit_reconciler`,
		`GRANT USAGE ON SCHEMA public TO audit_app, audit_reconciler`,
	} {
		if _, err := seed.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	return Roles{
		SuperDSN:      superDSN,
		AppDSN:        withUser(t, superDSN, "audit_app", AppRolePassword),
		ReconcilerDSN: withUser(t, superDSN, "audit_reconciler", ReconcilerRolePassword),
	}
}

func withUser(t testing.TB, dsn, user, password string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.User = url.UserPassword(user, password)
	return u.String()
}
