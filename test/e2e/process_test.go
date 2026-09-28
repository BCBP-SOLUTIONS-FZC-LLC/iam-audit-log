//go:build e2e

package e2e_test

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/test/fixtures"
)

// buildBinary compiles ./cmd/<name> from the repo root into a temp dir.
func buildBinary(t *testing.T, name string) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	require.NoError(t, err)
	out := filepath.Join(t.TempDir(), "iam-audit-log-"+name)
	cmd := exec.Command("go", "build", "-o", out, "./cmd/"+name)
	cmd.Dir = root
	b, err := cmd.CombinedOutput()
	require.NoError(t, err, string(b))
	return out
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()
	return fmt.Sprint(l.Addr().(*net.TCPAddr).Port)
}

// productionEnv is a complete APP_ENV=production environment (every
// fail-fast requirement of LLD §12 satisfied) for the given role DSNs.
func productionEnv(roles fixtures.Roles, appPort, metricsPort string) []string {
	env := []string{
		"APP_ENV=production",
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"DATABASE_URL=" + roles.AppDSN,
		"MIGRATION_DATABASE_URL=" + roles.SuperDSN,
		"APP_PORT=" + appPort,
		"METRICS_PORT=" + metricsPort,
		"PG_SSLMODE=disable",
	}
	for _, q := range config.InboundQueues() {
		env = append(env, q.EnvVar+"=http://sqs.local/000000000000/"+q.Name)
	}
	return env
}

func waitHTTP(t *testing.T, url string, want int) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == want {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("%s never returned %d", url, want)
}

// The real server binary, in production mode, authenticating as audit_app:
// boots, serves health + metrics, and drains to exit 0 on SIGTERM.
func TestServerBinary_ProductionBootAsAuditApp(t *testing.T) {
	bin := buildBinary(t, "server")
	roles := fixtures.CreateRoles(t, fixtures.StartPostgres(t))
	appPort, metricsPort := freePort(t), freePort(t)

	var logs bytes.Buffer
	cmd := exec.Command(bin)
	cmd.Env = productionEnv(roles, appPort, metricsPort)
	cmd.Stdout, cmd.Stderr = &logs, &logs
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	waitHTTP(t, "http://127.0.0.1:"+appPort+"/healthz", http.StatusOK)
	waitHTTP(t, "http://127.0.0.1:"+appPort+"/readyz", http.StatusOK)
	waitHTTP(t, "http://127.0.0.1:"+metricsPort+"/metrics", http.StatusOK)
	// Docs are hidden in production unless DOCS_ENABLED.
	waitHTTP(t, "http://127.0.0.1:"+appPort+"/asyncapi.yaml", http.StatusNotFound)

	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		assert.NoError(t, err, logs.String())
	case <-time.After(40 * time.Second):
		t.Fatalf("server did not drain:\n%s", logs.String())
	}
}

// Rule 5 / AL-INV-3: in production the server refuses to start as any role
// other than audit_app — here, the BYPASSRLS reconciler role.
func TestServerBinary_RefusesReconcilerRole(t *testing.T) {
	bin := buildBinary(t, "server")
	roles := fixtures.CreateRoles(t, fixtures.StartPostgres(t))
	env := productionEnv(roles, freePort(t), freePort(t))
	for i, kv := range env {
		if strings.HasPrefix(kv, "DATABASE_URL=") {
			env[i] = "DATABASE_URL=" + roles.ReconcilerDSN
		}
	}
	cmd := exec.Command(bin)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "server must exit non-zero")
	assert.Contains(t, string(out), "app DB role check failed")
}

// Smoke-gate parity: both binaries exit non-zero with no configuration.
func TestBinaries_ExitNonZeroWithoutConfig(t *testing.T) {
	for _, name := range []string{"server", "reconciler"} {
		cmd := exec.Command(buildBinary(t, name))
		cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
		out, err := cmd.CombinedOutput()
		assert.Error(t, err, "%s: %s", name, out)
	}
}

// An unknown --job is rejected with the list of valid jobs.
func TestReconcilerBinary_RejectsUnknownJob(t *testing.T) {
	cmd := exec.Command(buildBinary(t, "reconciler"), "--job=nope")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	require.Error(t, err)
	// Reported through the platform-gincommon logger (gap 43) as a structured
	// line: the message, the rejected job, and the valid ones.
	assert.Contains(t, string(out), "unknown job")
	assert.Contains(t, string(out), "nope")
	assert.Contains(t, string(out), "reconcile")
}

// The real reconciler binary, in production mode as audit_reconciler, runs
// the reconcile CronJob to completion (Phase 1 step: partition
// pre-creation) and exits 0.
func TestReconcilerBinary_ReconcileJobAsAuditReconciler(t *testing.T) {
	bin := buildBinary(t, "reconciler")
	roles := fixtures.CreateRoles(t, fixtures.StartPostgres(t))
	// cmd/server owns migrations; apply them the way it would.
	require.NoError(t, runServerMigrations(roles.SuperDSN))

	cmd := exec.Command(bin, "--job=reconcile")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "APP_ENV=production", "PG_SSLMODE=disable",
		"RECONCILER_DATABASE_URL=" + roles.ReconcilerDSN,
	}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	assert.Contains(t, string(out), "reconciler complete")
}

func runServerMigrations(dsn string) error {
	return pgadapter.RunMigrations(context.Background(), dsn, nil)
}
