//go:build integration

package main

import (
	"context"
	"errors"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/cmd/reconciler/jobs"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/test/fixtures"
)

// Drives the real run() against PostgreSQL 15 with stub jobs: the job gets
// the audit_reconciler pool and the configured knobs, and its outcome maps
// to the CronJob exit code.
func TestRun_DispatchesJobAgainstPostgres(t *testing.T) {
	dsn := fixtures.StartPostgres(t)
	t.Setenv("RECONCILER_DATABASE_URL", dsn)
	t.Setenv("PG_SSLMODE", "disable")

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

	t.Run("production: wrong DB role is fatal (rule 5)", func(t *testing.T) {
		t.Setenv("APP_ENV", "production")
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
