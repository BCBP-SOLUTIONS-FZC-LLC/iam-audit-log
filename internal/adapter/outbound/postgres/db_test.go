package postgres

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/puddle/v2"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	pgdomain "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/domain"
)

func isUnavailable(err error) bool {
	var de *domain.Error
	return errors.As(err, &de) && de.Code == domain.ErrDependencyUnavailable
}

// Connectivity-class failures map to 503 dependency_unavailable (§17);
// integrity and business errors pass through unchanged.
func TestWrapConnErr(t *testing.T) {
	unavailable := []error{
		&pgconn.PgError{Code: "08006", Message: "connection failure"},
		&pgconn.PgError{Code: "53300", Message: "too many connections"},
		&pgconn.PgError{Code: "57P01", Message: "admin shutdown"},
		&pgconn.PgError{Code: "58030", Message: "io error"},
		puddle.ErrClosedPool,
		io.EOF,
		fmt.Errorf("read: %w", io.ErrUnexpectedEOF),
		&net.OpError{Op: "dial", Err: errors.New("x")},
		fmt.Errorf("w: %w", syscall.ECONNRESET),
		errors.New("dial tcp: connection refused"),
		errors.New("write: broken pipe"),
		errors.New("read: connection reset by peer"),
	}
	for _, err := range unavailable {
		if !isUnavailable(wrapConnErr(err)) {
			t.Errorf("%v: want dependency_unavailable", err)
		}
	}

	passthrough := []error{
		&pgconn.PgError{Code: "23505", Message: "unique_violation"},
		&pgconn.PgError{Code: "42501", Message: "insufficient_privilege"},
		errors.New("business rule"),
		fmt.Errorf("w: %w", syscall.ENOENT),
	}
	for _, err := range passthrough {
		if got := wrapConnErr(err); got != err || isUnavailable(got) {
			t.Errorf("%v: must pass through unchanged, got %v", err, got)
		}
	}

	de := domain.NewError(domain.ErrUnknownEntryType, "x")
	if wrapConnErr(de) != de {
		t.Error("domain errors must pass through")
	}
	if wrapConnErr(nil) != nil {
		t.Error("nil must stay nil")
	}
}

// AL-INV-3: the app pool always takes pgcommon's transaction-local GUC path,
// even when PG_BOUNCER_MODE=false (direct Postgres in dev).
func TestAppPoolConfig_ForcesTransactionLocalGUC_ALINV3(t *testing.T) {
	t.Setenv("PG_BOUNCER_MODE", "false")
	cfg, _ := AppPoolConfig("postgres://app", nil, nil)
	if !cfg.PGBouncerMode {
		t.Fatal("PGBouncerMode must be forced true so app.tenant_id is set_config(..., true)")
	}
}

func TestPoolConfigs(t *testing.T) {
	log := &recLogger{}
	tr := NewOTelTracer("svc")

	app, _ := AppPoolConfig("postgres://app", log, tr)
	if app.DSN != "postgres://app" || app.GUCProvider == nil || !app.PGBouncerMode || app.Tracer == nil || app.Logger == nil {
		t.Errorf("app pool config: %+v", app)
	}
	appNoTracer, _ := AppPoolConfig("postgres://app", nil, nil)
	if appNoTracer.Tracer != nil || appNoTracer.Logger != nil {
		t.Error("nil tracer/logger must stay unset")
	}

	rec, _ := ReconcilerPoolConfig("postgres://recon", log, tr)
	if rec.DSN != "postgres://recon" || rec.GUCProvider != nil || !rec.PGBouncerMode || rec.Tracer == nil || rec.Logger == nil {
		t.Errorf("reconciler pool config must bind no GUC and force PgBouncer mode: %+v", rec)
	}
	recBare, _ := ReconcilerPoolConfig("postgres://recon", nil, nil)
	if recBare.Tracer != nil || recBare.Logger != nil {
		t.Error("nil tracer/logger must stay unset")
	}
}

func TestLoggerAdapter_ForwardsAllLevels(t *testing.T) {
	rec := &recLogger{}
	a := NewLoggerAdapter(rec)
	f := pgdomain.Field{Key: "k", Value: 1}
	a.Debug("d", f)
	a.Info("i", f)
	a.Warn("w", f)
	a.Error("e", f)
	want := []string{"debug:d", "info:i", "warn:w", "error:e"}
	for i, w := range want {
		if rec.entries[i] != w {
			t.Errorf("entry %d = %s, want %s", i, rec.entries[i], w)
		}
	}
	if m := fieldMap([]pgdomain.Field{f}); m["k"] != 1 {
		t.Errorf("fieldMap = %v", m)
	}
}

func TestOTelTracer(t *testing.T) {
	for _, name := range []string{"", "iam-audit-log"} {
		ctx, end := NewOTelTracer(name).StartSpan(context.Background(), "q")
		if ctx == nil {
			t.Fatal("nil ctx")
		}
		end()
	}
}

func TestWithTx_RoundTrip(t *testing.T) {
	if _, ok := TxFromContext(context.Background()); ok {
		t.Fatal("empty ctx must carry no tx")
	}
	ctx := WithTx(context.Background(), nil)
	if _, ok := TxFromContext(ctx); ok {
		t.Fatal("a nil tx must not be reported as present")
	}
}
