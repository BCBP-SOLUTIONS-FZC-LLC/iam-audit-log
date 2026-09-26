//go:build integration

package postgres

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/test/fixtures"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
)

func openPool(ctx context.Context, t *testing.T, dsn string) *pgcommon.Pool {
	t.Helper()
	cfg, _ := AppPoolConfig(dsn, &recLogger{}, NewOTelTracer("test"))
	pool, err := pgcommon.NewPool(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestPostgresAdapter_Integration(t *testing.T) {
	dsn := fixtures.StartPostgres(t)
	ctx := context.Background()
	pool := openPool(ctx, t, dsn)

	t.Run("CurrentRole reports the session role", func(t *testing.T) {
		ri, err := CurrentRole(ctx, pool)
		if err != nil {
			t.Fatal(err)
		}
		if ri.Name != "postgres" || !ri.BypassRLS {
			t.Errorf("role = %+v (testcontainers superuser expected)", ri)
		}
		if CheckAppRole(ri, "audit_app") == nil {
			t.Error("superuser must fail the audit_app check")
		}
	})

	t.Run("RunInTx commits and withPool joins the ctx transaction", func(t *testing.T) {
		runner := NewTxRunner(pool)
		if err := withPool(ctx, pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `CREATE TABLE probe (v int)`)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		err := runner.RunInTx(ctx, func(txCtx context.Context) error {
			if _, ok := TxFromContext(txCtx); !ok {
				t.Error("RunInTx must expose the tx on the context")
			}
			return withPool(txCtx, pool, func(tx pgx.Tx) error {
				_, err := tx.Exec(txCtx, `INSERT INTO probe VALUES (1)`)
				return err
			})
		})
		if err != nil {
			t.Fatal(err)
		}
		var n int
		if err := withPool(ctx, pool, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM probe`).Scan(&n)
		}); err != nil || n != 1 {
			t.Fatalf("count=%d err=%v", n, err)
		}
	})

	t.Run("RunInTx rolls back and passes business errors through", func(t *testing.T) {
		boom := errors.New("business rule")
		err := NewTxRunner(pool).RunInTx(ctx, func(txCtx context.Context) error {
			_ = withPool(txCtx, pool, func(tx pgx.Tx) error {
				_, err := tx.Exec(txCtx, `INSERT INTO probe VALUES (2)`)
				return err
			})
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the business error unchanged", err)
		}
		var n int
		_ = withPool(ctx, pool, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM probe WHERE v = 2`).Scan(&n)
		})
		if n != 0 {
			t.Fatal("rolled-back insert is visible")
		}
	})

	t.Run("runMigrations applies a migration set idempotently; the embedded set applies", func(t *testing.T) {
		// Synthetic set in its own database so its version history cannot
		// collide with the embedded migrations.
		probeDSN := createDatabase(ctx, t, pool, dsn, "migrate_probe")
		fsys := fstest.MapFS{
			"000001_probe.up.sql":   {Data: []byte(`CREATE TABLE migrated_probe (id int);`)},
			"000001_probe.down.sql": {Data: []byte(`DROP TABLE migrated_probe;`)},
		}
		if err := runMigrations(ctx, fsys, probeDSN, &recLogger{}); err != nil {
			t.Fatal(err)
		}
		if err := runMigrations(ctx, fsys, probeDSN, nil); err != nil {
			t.Fatalf("re-run must be idempotent: %v", err)
		}
		if err := runMigrations(ctx, fstest.MapFS{}, probeDSN, nil); err != nil {
			t.Fatal(err)
		}
		if err := RunMigrations(ctx, dsn, &recLogger{}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a closed pool maps to dependency_unavailable", func(t *testing.T) {
		p := openPool(ctx, t, dsn)
		p.Close()
		if _, err := CurrentRole(ctx, p); !isUnavailable(err) {
			t.Fatalf("err = %v, want dependency_unavailable", err)
		}
	})
}

// createDatabase creates name on the same server and returns its DSN.
func createDatabase(ctx context.Context, t *testing.T, pool *pgcommon.Pool, dsn, name string) string {
	t.Helper()
	if err := pool.WithConn(ctx, func(ctx context.Context, c *pgxpool.Conn) error {
		_, err := c.Exec(ctx, "CREATE DATABASE "+name)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}
