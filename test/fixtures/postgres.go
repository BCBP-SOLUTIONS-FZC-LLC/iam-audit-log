//go:build integration || e2e

// Package fixtures holds shared test harness helpers. StartPostgres boots a
// PostgreSQL 15 testcontainer (the platform RDS major version) and returns
// its superuser DSN — mirroring the sibling IAM services' setupTestDB
// (retry ×3, wait for the second "ready" log line).
package fixtures

import (
	"context"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// PostgresImage is the platform's RDS major version (LLD §3 stack).
const PostgresImage = "postgres:15-alpine"

// StartPostgres starts a disposable `audit` database and returns the
// superuser DSN. The container is terminated when t finishes.
func StartPostgres(t testing.TB) string {
	t.Helper()
	if testing.Short() {
		t.Skip("postgres testcontainer skipped in -short mode")
	}
	ctx := context.Background()
	var (
		c   *tcpostgres.PostgresContainer
		dsn string
		err error
	)
	// On a loaded Docker host the "ready" log line can come before the port
	// mapping is published ("port 5432/tcp not found"), so the wait includes
	// the listening port, and a container whose DSN still cannot be resolved
	// counts as a failed attempt: it is terminated and a fresh one started.
	for attempt := 1; attempt <= 3; attempt++ {
		c, err = tcpostgres.Run(ctx, PostgresImage,
			tcpostgres.WithDatabase("audit"),
			tcpostgres.WithUsername("postgres"),
			tcpostgres.WithPassword("testpassword"),
			testcontainers.WithWaitStrategy(wait.ForAll(
				wait.ForLog("database system is ready to accept connections").WithOccurrence(2),
				wait.ForListeningPort("5432/tcp"),
			).WithDeadline(90*time.Second)),
		)
		if err == nil {
			dsn, err = c.ConnectionString(ctx, "sslmode=disable")
			if err == nil {
				break
			}
			_ = c.Terminate(ctx) //nolint:errcheck // discarding a half-started container before retrying
			c = nil
		}
		time.Sleep(time.Duration(attempt) * time.Second)
	}
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Terminate(context.Background()); err != nil {
			t.Logf("terminate postgres container: %v", err)
		}
	})
	return dsn
}
