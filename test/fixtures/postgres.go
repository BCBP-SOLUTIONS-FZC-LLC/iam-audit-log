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
		err error
	)
	for attempt := 1; attempt <= 3; attempt++ {
		c, err = tcpostgres.Run(ctx, PostgresImage,
			tcpostgres.WithDatabase("audit"),
			tcpostgres.WithUsername("postgres"),
			tcpostgres.WithPassword("testpassword"),
			testcontainers.WithWaitStrategy(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).WithStartupTimeout(60*time.Second)),
		)
		if err == nil {
			break
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
	dsn, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("postgres dsn: %v", err)
	}
	return dsn
}
