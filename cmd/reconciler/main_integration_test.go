//go:build integration

package main

import (
	"context"
	"errors"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
	"github.com/stretchr/testify/require"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/cmd/reconciler/jobs"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/test/fixtures"
)

// Drives the real run() against PostgreSQL 15 with stub jobs: the job gets
// the audit_reconciler pool and the configured knobs, and its outcome maps
// to the CronJob exit code.
func TestRun_DispatchesJobAgainstPostgres(t *testing.T) {
	dsn := fixtures.StartPostgres(t)
	// cmd/server migrates in production; the run claims its job lease in
	// reconciler_job_lease (migration 000011).
	if err := pgadapter.RunMigrations(context.Background(), dsn, nil); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RECONCILER_DATABASE_URL", dsn)

	var got *jobs.Context
	stubRegistry(t, map[string]jobs.Func{
		"ok": func(ctx context.Context, jctx *jobs.Context) (jobs.Result, error) {
			got = jctx
			return jobs.Result{Attempted: 1, Succeeded: 1}, jctx.Pool.Ping(ctx)
		},
		"fail": func(context.Context, *jobs.Context) (jobs.Result, error) {
			return jobs.Result{}, errors.New("job failed")
		},
	})

	t.Run("dev: role mismatch tolerated, job succeeds → exit 0", func(t *testing.T) {
		t.Setenv("APP_ENV", "test")
		withArgs(t, "--job=ok")
		if code := run(); code != 0 {
			t.Fatalf("exit %d, want 0", code)
		}
		if got == nil || got.Pool == nil || got.HotWindowDays != 90 || got.ProcessedEventsTTLDays != 8 {
			t.Fatalf("job context = %+v", got)
		}
	})

	t.Run("job error → exit 1", func(t *testing.T) {
		t.Setenv("APP_ENV", "test")
		withArgs(t, "--job=fail")
		if code := run(); code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
	})

	// APP_ENV=prod is a different metrics identity: its own process.
	t.Run("production: wrong DB role is fatal (rule 5)", func(t *testing.T) {
		if !fixtures.InOwnProcess(t) {
			return
		}
		t.Setenv("APP_ENV", "prod")
		withArgs(t, "--job=ok")
		if code := run(); code != 1 {
			t.Fatalf("exit %d, want 1 — superuser is not audit_reconciler", code)
		}
	})

	t.Run("unreachable database → exit 1", func(t *testing.T) {
		t.Setenv("APP_ENV", "test")
		t.Setenv("RECONCILER_DATABASE_URL", "postgres://nobody:x@127.0.0.1:1/audit?sslmode=disable&connect_timeout=2")
		withArgs(t, "--job=ok")
		if code := run(); code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
	})
}

// Migration 000011: a run whose job lease is held by another run (e.g. a
// manual Job beside the scheduled one) is skipped, exits 0 so the CronJob is
// not marked failed, and never runs the job; without the lease table the run
// fails instead of running unguarded.
func TestRun_JobLease(t *testing.T) {
	dsn := fixtures.StartPostgres(t)
	if err := pgadapter.RunMigrations(context.Background(), dsn, nil); err != nil {
		t.Fatal(err)
	}
	t.Setenv("APP_ENV", "test")
	t.Setenv("RECONCILER_DATABASE_URL", dsn)
	ran := false
	stubRegistry(t, map[string]jobs.Func{
		"guarded": func(context.Context, *jobs.Context) (jobs.Result, error) { ran = true; return jobs.Result{}, nil },
	})

	cfg, _ := pgadapter.ReconcilerPoolConfig(dsn, nil, nil)
	pool, err := pgcommon.NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	lease := pgadapter.NewJobLease(pool)

	t.Run("held by another run → skipped, exit 0", func(t *testing.T) {
		if ok, err := lease.Claim(context.Background(), "guarded", "other-run", time.Hour); err != nil || !ok {
			t.Fatalf("pre-claim: %v %v", ok, err)
		}
		withArgs(t, "--job=guarded")
		if code := run(); code != 0 {
			t.Fatalf("exit %d, want 0", code)
		}
		if ran {
			t.Fatal("the job ran while another run held its lease")
		}
		require.NoError(t, lease.Release(context.Background(), "guarded", "other-run"))
	})

	t.Run("free → runs and releases", func(t *testing.T) {
		withArgs(t, "--job=guarded")
		if code := run(); code != 0 || !ran {
			t.Fatalf("exit %d, ran %v", code, ran)
		}
		ok, err := lease.Claim(context.Background(), "guarded", "next-run", time.Minute)
		if err != nil || !ok {
			t.Fatalf("the finished run must release its lease: %v %v", ok, err)
		}
	})

	t.Run("lease table missing → exit 1", func(t *testing.T) {
		bare := fixtures.StartPostgres(t) // never migrated
		t.Setenv("RECONCILER_DATABASE_URL", bare)
		ran = false
		withArgs(t, "--job=guarded")
		if code := run(); code != 1 || ran {
			t.Fatalf("exit %d ran %v, want 1 and not run", code, ran)
		}
	})
}
