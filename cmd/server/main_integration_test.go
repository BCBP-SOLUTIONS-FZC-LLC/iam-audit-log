//go:build integration

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/test/fixtures"
)

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return fmt.Sprint(l.Addr().(*net.TCPAddr).Port)
}

// Boots the real composition root against PostgreSQL 15: migrations run,
// the audit_app pool connects, /healthz and /readyz answer, and SIGTERM
// drains to exit code 0. signal.Notify in run() pre-empts the default
// terminate action, so self-signaling is safe in-process.
func TestServer_BootsServesAndDrains(t *testing.T) {
	dsn := fixtures.StartPostgres(t)
	appPort, metricsPort := freePort(t), freePort(t)
	t.Setenv("APP_ENV", "test")
	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("MIGRATION_DATABASE_URL", dsn)
	t.Setenv("APP_PORT", appPort)
	t.Setenv("METRICS_PORT", metricsPort)

	done := make(chan int, 1)
	go func() { done <- run() }()

	base := "http://127.0.0.1:" + appPort
	waitFor(t, done, base+"/healthz", http.StatusOK)
	waitFor(t, done, base+"/readyz", http.StatusOK)
	waitFor(t, done, "http://127.0.0.1:"+metricsPort+"/metrics", http.StatusOK)

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("run() exit code %d, want 0", code)
		}
	case <-time.After(40 * time.Second):
		t.Fatal("server did not drain within 40s")
	}
}

// waitFor polls url until it returns want, failing fast if run() exits.
func waitFor(t *testing.T, done <-chan int, url string, want int) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case code := <-done:
			t.Fatalf("run() exited early with code %d while waiting for %s", code, url)
		default:
		}
		resp, err := http.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == want {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("%s never returned %d", url, want)
}

func setDevEnv(t *testing.T, dsn string) {
	t.Helper()
	t.Setenv("APP_ENV", "test")
	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("MIGRATION_DATABASE_URL", dsn)
	t.Setenv("APP_PORT", freePort(t))
	t.Setenv("METRICS_PORT", freePort(t))
}

// Startup failure paths each exit 1 (the smoke gate relies on non-zero).
func TestServer_StartupFailuresExit1(t *testing.T) {
	dsn := fixtures.StartPostgres(t)

	t.Run("migration failure", func(t *testing.T) {
		setDevEnv(t, dsn)
		orig := runMigrations
		runMigrations = func(context.Context, string, port.Logger) error { return errors.New("dirty") }
		t.Cleanup(func() { runMigrations = orig })
		if code := run(); code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
	})

	t.Run("invalid partition window", func(t *testing.T) {
		setDevEnv(t, dsn)
		t.Setenv("AUDIT_PRECREATE_MONTHS", "30")
		if code := run(); code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
	})

	t.Run("unreachable database", func(t *testing.T) {
		setDevEnv(t, "postgres://nobody:x@127.0.0.1:1/audit?sslmode=disable&connect_timeout=2")
		if code := run(); code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
	})

	// Outside dev the app pool must authenticate as audit_app without
	// BYPASSRLS; the testcontainers superuser must be refused (AL-INV-3).
	t.Run("production refuses a non-audit_app role", func(t *testing.T) {
		setDevEnv(t, dsn)
		t.Setenv("APP_ENV", "production")
		for _, q := range config.InboundQueues() {
			t.Setenv(q.EnvVar, "http://sqs.local/"+q.Name)
		}
		if code := run(); code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
	})
}

// A listener that cannot bind triggers an orderly shutdown instead of a
// half-alive process.
func TestServer_PortInUseShutsDownCleanly(t *testing.T) {
	dsn := fixtures.StartPostgres(t)
	setDevEnv(t, dsn)
	busy, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	port := fmt.Sprint(busy.Addr().(*net.TCPAddr).Port)
	t.Setenv("APP_PORT", port)
	t.Setenv("METRICS_PORT", port)

	done := make(chan int, 1)
	go func() { done <- run() }()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d, want 0 (orderly shutdown)", code)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("server did not shut down after listener failure")
	}
}

// Partition pre-creation failing at startup is logged, not fatal: the
// DEFAULT partition keeps ingestion working (LLD §4.4). Simulated by
// skipping migrations on a fresh database, so audit_ensure_partitions()
// does not exist yet.
func TestServer_StartupPartitionFailureIsNotFatal(t *testing.T) {
	dsn := fixtures.StartPostgres(t)
	setDevEnv(t, dsn)
	orig := runMigrations
	runMigrations = func(context.Context, string, port.Logger) error { return nil }
	t.Cleanup(func() { runMigrations = orig })

	done := make(chan int, 1)
	go func() { done <- run() }()
	waitFor(t, done, "http://127.0.0.1:"+os.Getenv("APP_PORT")+"/readyz", http.StatusOK)
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
}
