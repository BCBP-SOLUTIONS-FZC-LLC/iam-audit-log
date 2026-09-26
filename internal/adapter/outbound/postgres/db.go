// Package postgres implements the outbound repository ports backed by the
// `audit` PostgreSQL database through platform-pgcommon. Raw SQL is confined
// to this package (LLD §3.2).
//
// Two pool shapes exist, one per composition root (LLD §3.3.2, §10.4):
//
//   - AppPoolConfig (cmd/server, role audit_app): GUCProvider bound and
//     PgBouncer mode forced, so every transaction issues
//     set_config('app.tenant_id', …, true) — transaction-local, never
//     session-level. INSERT+SELECT only.
//   - ReconcilerPoolConfig (cmd/reconciler, role audit_reconciler): no
//     GUCProvider — BYPASSRLS cross-tenant archival/pruning, the sole DELETE /
//     partition-DDL path.
package postgres

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/puddle/v2"
)

// AppPoolConfig returns the pgcommon.Config for cmd/server's audit_app pool.
// Pool sizing comes from pgcommon.ConfigFromEnv (PG_* vars); the DSN is the
// explicitly-passed app DSN.
//
// PGBouncerMode is FORCED true, whatever PG_BOUNCER_MODE says: pgcommon only
// binds the GUC set transaction-locally (set_config(…, true) right after
// BEGIN) in PgBouncer mode. With it off, pgcommon injects the GUCs
// session-level in a PrepareConn hook and never clears them, so a tenant's
// app.tenant_id survives COMMIT and leaks onto the next checkout of that
// backend (test/postgres TestGUC_* caught exactly this). AL-INV-3 and the
// LLD's "SET LOCAL inside each transaction, never session-level" rule
// require the transaction-local path in every environment, dev included.
func AppPoolConfig(dsn string, log port.Logger, tracer *OTelTracer) (pgcommon.Config, []pgcommon.ConfigWarning) {
	cfg, warnings := pgcommon.ConfigFromEnv()
	cfg.DSN = dsn
	cfg.PGBouncerMode = true
	cfg.GUCProvider = pgcommon.GUCSetFromContext
	if tracer != nil {
		cfg.Tracer = tracer
	}
	if log != nil {
		cfg.Logger = NewLoggerAdapter(log)
	}
	return cfg, warnings
}

// ReconcilerPoolConfig returns the pgcommon.Config for cmd/reconciler's
// audit_reconciler pool. No GUCProvider (BYPASSRLS), and PGBouncerMode is
// forced true because the pool still connects through PgBouncer in
// production (matching the siblings' SystemPoolConfig).
func ReconcilerPoolConfig(dsn string, log port.Logger, tracer *OTelTracer) (pgcommon.Config, []pgcommon.ConfigWarning) {
	cfg, warnings := pgcommon.ConfigFromEnv()
	cfg.DSN = dsn
	cfg.GUCProvider = nil
	cfg.PGBouncerMode = true
	if tracer != nil {
		cfg.Tracer = tracer
	}
	if log != nil {
		cfg.Logger = NewLoggerAdapter(log)
	}
	return cfg, warnings
}

// TxRunner wraps pgcommon.Pool to run a unit of work in one transaction.
// Every ingestion (bus or direct-write) is a single RunInTx (LLD §9).
type TxRunner struct {
	pool *pgcommon.Pool
}

// NewTxRunner constructs a TxRunner over pool.
func NewTxRunner(pool *pgcommon.Pool) *TxRunner {
	return &TxRunner{pool: pool}
}

// writeRetryOpts is pgcommon's documented OLTP preset: deadlock (40P01) and
// serialization failure (40001) retry with backoff + jitter.
var writeRetryOpts = pgcommon.RetryOptions{
	MaxAttempts:    3,
	InitialWait:    10 * time.Millisecond,
	MaxWait:        500 * time.Millisecond,
	Multiplier:     2.0,
	JitterFraction: 0.25,
}

// RunInTx runs fn inside a transaction whose context carries the pgx.Tx, so
// repository helpers join it. Connectivity failures map to
// domain.ErrDependencyUnavailable (503, §17).
func (r *TxRunner) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return wrapConnErr(pgcommon.RunInTxWithRetryOpts(ctx, r.pool, pgx.TxOptions{}, writeRetryOpts, func(ctx context.Context, tx pgx.Tx) error {
		return fn(WithTx(ctx, tx))
	}))
}

type txKey struct{}

// WithTx stores the active pgx.Tx in ctx.
func WithTx(ctx context.Context, tx pgx.Tx) context.Context {
	return context.WithValue(ctx, txKey{}, tx)
}

// TxFromContext retrieves the active pgx.Tx set by RunInTx, if any.
func TxFromContext(ctx context.Context) (pgx.Tx, bool) {
	tx, ok := ctx.Value(txKey{}).(pgx.Tx)
	return tx, ok
}

// withPool runs fn in the ctx transaction if present, else in a fresh
// pgcommon.RunInTx (which still binds the RLS GUC via the checkout hook).
func withPool(ctx context.Context, pool *pgcommon.Pool, fn func(pgx.Tx) error) error {
	if tx, ok := TxFromContext(ctx); ok {
		return wrapConnErr(fn(tx))
	}
	return wrapConnErr(pgcommon.RunInTx(ctx, pool, pgx.TxOptions{}, func(_ context.Context, tx pgx.Tx) error {
		return fn(tx)
	}))
}

// wrapConnErr converts positively-identified connectivity/resource failures
// (SQLSTATE 08/53/57/58, a closed pool, Go-level network errors) into
// domain.ErrDependencyUnavailable. Everything else — SQL integrity errors
// and a callback's own business errors — passes through unchanged so the
// service layer can classify it (mirrors iam-realm-provisioner).
func wrapConnErr(err error) error {
	if err == nil {
		return nil
	}
	var de *domain.Error
	if errors.As(err, &de) {
		return err
	}
	if pgcommon.IsConnectionException(err) || pgcommon.IsInsufficientResources(err) ||
		isOperatorOrSystemErrorSQLState(err) || errors.Is(err, puddle.ErrClosedPool) || isNetworkError(err) {
		return domain.NewError(domain.ErrDependencyUnavailable, "database unavailable")
	}
	return err
}

func isNetworkError(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		return true
	}
	var sysErr syscall.Errno
	if errors.As(err, &sysErr) {
		switch sysErr { //nolint:exhaustive // only transient-connection codes
		case syscall.ECONNRESET, syscall.ECONNREFUSED, syscall.EPIPE, syscall.ETIMEDOUT:
			return true
		}
	}
	msg := err.Error()
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "broken pipe")
}

// isOperatorOrSystemErrorSQLState reports SQLSTATE class 57/58; pgcommon
// v1.3.0 has helpers for 08/53 only.
func isOperatorOrSystemErrorSQLState(err error) bool {
	if !pgcommon.IsPgError(err) {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "SQLSTATE 57") || strings.Contains(msg, "SQLSTATE 58")
}
