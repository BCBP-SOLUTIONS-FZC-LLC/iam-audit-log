package postgres

import (
	"context"
	"embed"
	"io/fs"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
	pgmigrate "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/migrate"
)

// migrationsFS embeds every migration. Files are numbered
// NNNNNN_<name>.up.sql / .down.sql (golang-migrate convention, LLD §4.4);
// every up has a paired down so each step is individually reversible.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// MigrationsFS returns the embedded migrations rooted at the migrations dir
// (used by the runner and by the postgres test harness).
func MigrationsFS() fs.FS {
	sub, _ := fs.Sub(migrationsFS, "migrations") //nolint:errcheck // infallible for an embedded, known directory
	return sub
}

// RunMigrations applies all pending migrations against dsn, which must be
// the audit_migrator DSN connecting directly to Postgres (the runner's
// advisory lock is session-scoped and breaks under PgBouncer transaction
// pooling). Tracked in pgcommon's default pgcommon_migrations table.
//
// There is deliberately NO outbox.ApplySchema step before this: the service
// owns no outbox (LLD §4.4, AL-INV-10).
func RunMigrations(ctx context.Context, dsn string, log port.Logger) error {
	return runMigrations(ctx, MigrationsFS(), dsn, log)
}

// runMigrations applies fsys's migrations; split out so tests can drive the
// real runner against a synthetic migration set.
func runMigrations(ctx context.Context, fsys fs.FS, dsn string, log port.Logger) error {
	ups, _ := fs.Glob(fsys, "*.up.sql") //nolint:errcheck // pattern is static and valid
	if len(ups) == 0 {
		// pgcommon's runner rejects an empty source; an empty set is a no-op.
		if log != nil {
			log.Info("migrations: none embedded — nothing to apply", nil)
		}
		return nil
	}
	runner := &pgmigrate.Runner{FS: fsys, DSN: dsn}
	if log != nil {
		runner.Logger = NewLoggerAdapter(log)
	}
	return runner.Up(ctx)
}
