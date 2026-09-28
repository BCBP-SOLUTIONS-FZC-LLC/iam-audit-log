package main

import (
	"context"
	"os"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/cmd/reconciler/jobs"
)

func withArgs(t *testing.T, args ...string) {
	t.Helper()
	orig := os.Args
	os.Args = append([]string{"reconciler"}, args...)
	t.Cleanup(func() { os.Args = orig })
}

func stubRegistry(t *testing.T, fns map[string]jobs.Func) {
	t.Helper()
	orig := jobRegistry
	jobRegistry = func() map[string]jobs.Func { return fns }
	t.Cleanup(func() { jobRegistry = orig })
}

func noop(context.Context, *jobs.Context) (jobs.Result, error) { return jobs.Result{}, nil }

func TestRun_RequiresAJob(t *testing.T) {
	withArgs(t)
	t.Setenv("RECONCILER_JOB", "")
	if code := run(); code != 1 {
		t.Fatalf("no job: exit %d, want 1", code)
	}
}

func TestRun_RejectsUnknownJob(t *testing.T) {
	stubRegistry(t, map[string]jobs.Func{"b-job": noop, "a-job": noop})
	withArgs(t, "--job=nope")
	if code := run(); code != 1 {
		t.Fatalf("unknown job: exit %d, want 1", code)
	}
	if got := names(jobRegistry()); len(got) != 2 || got[0] != "a-job" {
		t.Errorf("names() = %v, want sorted", got)
	}
}

// Rule 5: the reconciler never falls back to the audit_app DSN.
func TestRun_RequiresReconcilerDSN(t *testing.T) {
	stubRegistry(t, map[string]jobs.Func{"x": noop})
	withArgs(t, "--job=x")
	t.Setenv("RECONCILER_DATABASE_URL", "")
	t.Setenv("DATABASE_URL", "postgres://app-role-must-not-be-used")
	if code := run(); code != 1 {
		t.Fatalf("missing RECONCILER_DATABASE_URL: exit %d, want 1", code)
	}
}

func TestRun_BadFlagExits2(t *testing.T) {
	withArgs(t, "--no-such-flag")
	if code := run(); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
}
