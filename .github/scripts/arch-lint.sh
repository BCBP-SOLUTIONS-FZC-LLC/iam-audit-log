#!/usr/bin/env bash
# Installs go-arch-lint if missing, then enforces .go-arch-lint.yml rules
# (LLD §3.2 — core imports no adapter/Gin/pgx/AWS SDK; adapters never import
# each other) plus the grep gates below. Mirrors iam-realm-provisioner.
set -euo pipefail

if ! command -v go-arch-lint >/dev/null; then
  go install github.com/fe3dback/go-arch-lint@v1.15.0
fi
go-arch-lint check --project-path .

fail() { echo "FAIL: $1"; echo "$2"; exit 1; }

# ── Observability: logs/metrics/traces via platform-gincommon only ─────────
echo "Checking observability invariant (platform-gincommon only)..."
o=$(grep -rlE '"go\.uber\.org/zap"|"github\.com/sirupsen/logrus"|^[[:space:]]*"log"$' --include="*.go" . 2>/dev/null | grep -v "_test.go" || true)
[ -z "$o" ] || fail "a logging library other than platform-gincommon's logger is imported:" "$o"
o=$(grep -rlE 'prometheus\.MustRegister\(|prometheus\.DefaultRegisterer\b' --include="*.go" . 2>/dev/null | grep -v "_test.go" || true)
[ -z "$o" ] || fail "a Prometheus collector is registered outside gincommon.MetricsRegisterer():" "$o"
o=$(grep -rlE 'otel\.SetTracerProvider\(|sdktrace\.NewTracerProvider\(|"go\.opentelemetry\.io/otel/sdk/trace"' --include="*.go" . 2>/dev/null | grep -v "_test.go" || true)
[ -z "$o" ] || fail "a TracerProvider is configured outside gincommon.InitTracingFromEnv():" "$o"
echo "OK"

# ── Database: every connection, configuration and operation goes through
# platform-pgcommon (tests included; BUILD_PLAN gap 44) ─────────────────────
#   connection:    pools only via pgcommon.NewPool; no other driver, no
#                  pgx.Connect / pgxpool.New* / ParseConfig (DSN parsing).
#   configuration: pool config only from pgcommon.ConfigFromEnv (production)
#                  — the service injects just the per-role DSN; PG_* tuning is
#                  never read by this repo; migrations only via
#                  platform-pgcommon/pkg/migrate.
#   operation:     transactions only via pgcommon (RunInTx*, Pool.WithTx,
#                  Pool.WithConn) — no raw Begin/BeginTx/Acquire; Postgres
#                  errors classified only via pgcommon.Is*/ConstraintName —
#                  no pgconn.PgError; raw SQL only in the postgres adapter.
echo "Checking database invariant (platform-pgcommon only)..."
all()  { grep -rlE "$1" --include="*.go" . 2>/dev/null || true; }
prod() { grep -rlE "$1" --include="*.go" cmd internal pkg api 2>/dev/null | grep -v "_test\.go" || true; }

o=$(all '"database/sql"|"github\.com/lib/pq"|"github\.com/jackc/pgx/v5/stdlib"')
[ -z "$o" ] || fail "a Postgres driver other than pgx-via-pgcommon is imported:" "$o"
o=$(all 'pgxpool\.(New|NewWithConfig|ParseConfig)\(|pgx\.(Connect|ConnectConfig|ParseConfig)\(|pgconn\.(Connect|ParseConfig)\(')
[ -z "$o" ] || fail "a connection/pool is constructed or configured outside pgcommon.NewPool:" "$o"
o=$(all '"github\.com/golang-migrate/migrate')
[ -z "$o" ] || fail "golang-migrate imported directly instead of platform-pgcommon/pkg/migrate:" "$o"
o=$(prod '"github\.com/jackc/pgx/v5' | grep -v "^internal/adapter/outbound/postgres/" || true)
[ -z "$o" ] || fail "pgx imported outside internal/adapter/outbound/postgres (raw SQL is confined there, LLD §3.2):" "$o"
o=$(prod '\.(Begin|BeginTx|BeginFunc|BeginTxFunc|Acquire|AcquireFunc)\(ctx')
[ -z "$o" ] || fail "a transaction/connection is opened directly instead of via pgcommon RunInTx/WithTx/WithConn:" "$o"
o=$(prod 'pgconn\.PgError')   # tests may fabricate PgErrors to simulate failures
[ -z "$o" ] || fail "a Postgres error is classified directly instead of via pgcommon.Is*/ConstraintName:" "$o"
o=$(prod 'pgcommon\.Config\{')
[ -z "$o" ] || fail "a pool config is built by hand in production code instead of from pgcommon.ConfigFromEnv:" "$o"
o=$(prod 'Getenv\("PG_|LookupEnv\("PG_')
[ -z "$o" ] || fail "a PG_* pool setting is read by this repo instead of by pgcommon.ConfigFromEnv:" "$o"
o=$(prod 'pgcommon\.ConfigFromEnv\(' | grep -v "^internal/adapter/outbound/postgres/" || true)
[ -z "$o" ] || fail "pgcommon.ConfigFromEnv called outside the postgres adapter's pool-config builders:" "$o"
echo "OK"

# ── Transport confinement (LLD §3.2) ───────────────────────────────────────
# SQS: only cmd/server/main.go builds a *sqs.Client, and only for the DLQ
# depth gauge; consumers are built by platform-events (events.NewSQSConsumer). S3: only the s3 adapter + composition
# roots (client construction). Glue: only the glue adapter + cmd/server.
# test/ is exempt: harnesses (test/fixtures, test/integration) play the
# external producer / AWS side — same exemption iam-org-membership's
# check-forbidden-events-bypass.sh makes for tests.
echo "Checking AWS SDK transport confinement..."
o=$(grep -rlE '"github\.com/aws/aws-sdk-go-v2/service/sqs"' --include="*.go" . 2>/dev/null | grep -v "_test.go" | grep -v "^\./test/" | grep -v "^\./cmd/server/main\.go$" || true)
[ -z "$o" ] || fail "aws-sdk-go-v2/service/sqs imported outside cmd/server/main.go:" "$o"
o=$(grep -rlE '"github\.com/aws/aws-sdk-go-v2/service/s3"' --include="*.go" . 2>/dev/null | grep -v "_test.go" | grep -v "^\./test/" | grep -vE "^\./(internal/adapter/outbound/s3/|cmd/server/main\.go$|cmd/reconciler/main\.go$)" || true)
[ -z "$o" ] || fail "aws-sdk-go-v2/service/s3 imported outside internal/adapter/outbound/s3 or a composition root:" "$o"
o=$(grep -rlE '"github\.com/aws/aws-sdk-go-v2/service/glue"' --include="*.go" . 2>/dev/null | grep -v "_test.go" | grep -v "^\./test/" | grep -vE "^\./(internal/adapter/outbound/glue/|cmd/server/main\.go$)" || true)
[ -z "$o" ] || fail "aws-sdk-go-v2/service/glue imported outside internal/adapter/outbound/glue or cmd/server/main.go:" "$o"
o=$(grep -rlE '^[[:space:]]*type[[:space:]]+Envelope[[:space:]]' --include="*.go" . 2>/dev/null | grep -v "_test.go" || true)
[ -z "$o" ] || fail "a local Envelope type competes with platform-events' events.Envelope:" "$o"
echo "OK"

# ── Role confinement (implementation rule 5, LLD §3.3.2/§10.4) ─────────────
# cmd/server never reads the reconciler DSN; cmd/reconciler never reads the
# app DSN helper. Each composition root binds exactly its own DB role.
echo "Checking DB-role confinement per composition root..."
o=$(grep -rnE 'RECONCILER_DATABASE_URL|ReconcilerDSNFromEnv' --include="*.go" cmd/server 2>/dev/null | grep -v "_test.go" || true)
[ -z "$o" ] || fail "cmd/server references the audit_reconciler DSN (rule 5 — server must never acquire the reconciler role):" "$o"
o=$(grep -rnE 'AppDSNFromEnv|"DATABASE_URL"' --include="*.go" cmd/reconciler 2>/dev/null | grep -v "_test.go" || true)
[ -z "$o" ] || fail "cmd/reconciler references the audit_app DSN (rule 5):" "$o"
echo "OK"

# AL-INV-10 (no publisher/outbox) lives in check-forbidden-events-bypass.sh;
# the non-LOCAL SET app.tenant_id gate is inline in validate-quality.yml.
