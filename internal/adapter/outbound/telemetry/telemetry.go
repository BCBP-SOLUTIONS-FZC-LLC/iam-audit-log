// Package telemetry is the single seam between this service and the
// observability stack that platform-gincommon owns (rule: every log, metric
// and trace goes through platform-gincommon).
//
//   - Logs: built only by gincommon's logger.NewLogger (Zap), and passed
//     around as port.Logger. Nothing in this repository writes log lines any
//     other way; check-observability-confinement.sh enforces it.
//   - Metrics: every collector registers on gincommon.MetricsRegisterer()
//     (internal/adapter/outbound/metrics), and /metrics is served from that
//     same registry by gincommon.MetricsHandler.
//   - Traces: gincommon.InitTracingWithConfig / ObservabilityMiddlewares
//     install the process TracerProvider and OTLP pipeline. Tracer and
//     TraceID wrap gincommon's tracing API (NewSpanTracer, SpanTraceID), so
//     every span and log trace_id belongs to gincommon's pipeline.
//
// Since platform-gincommon v1.4.0 the span API, the /metrics handler and the
// context trace-id helper are gincommon's own, so this package no longer
// imports OpenTelemetry or promhttp (BUILD_PLAN gap 43); nothing in the
// repository does.
package telemetry

import (
	"context"
	"io"
	stdlog "log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

// Tracer implements port.Tracer (and pgcommon.Config.Tracer) on the
// TracerProvider gincommon installed.
type Tracer struct {
	*gincommon.SpanTracer
}

var _ port.Tracer = (*Tracer)(nil)

// NewTracer returns a tracer whose instrumentation scope is serviceName.
func NewTracer(serviceName string) *Tracer {
	if serviceName == "" {
		serviceName = "iam-audit-log"
	}
	return &Tracer{gincommon.NewSpanTracer(serviceName)}
}

// TraceID returns the active span's trace id, or "" (port.TraceIDFunc).
func TraceID(ctx context.Context) string { return gincommon.SpanTraceID(ctx) }

var _ port.TraceIDFunc = TraceID

// MetricsHandler serves exactly the registry behind
// gincommon.MetricsRegisterer(), so /metrics exposes gincommon's HTTP metrics
// and every collector registered on it, and nothing registered elsewhere.
// gincommon's handler continues past a failing collector instead of blanking
// the scrape.
func MetricsHandler() http.Handler { return gincommon.MetricsHandler() }

// HTTPErrorLog returns a standard-library *log.Logger for http.Server.ErrorLog
// that forwards each line to the gincommon logger at error level. Without it,
// net/http writes its own errors (TLS handshake, accept, handler panics)
// straight to stderr, bypassing gincommon (gap 43).
func HTTPErrorLog(l port.Logger, server string) *stdlog.Logger {
	return stdlog.New(logWriter{l: l, server: server}, "", 0)
}

type logWriter struct {
	l      port.Logger
	server string
}

func (w logWriter) Write(p []byte) (int, error) {
	if w.l != nil {
		w.l.Error("http server error", map[string]any{"server": w.server, "error": strings.TrimRight(string(p), "\n")})
	}
	return len(p), nil
}

// QuietGin routes gin's own console output away from stdout/stderr: release
// mode always (no debug route dump), and no default writers. Requests are
// logged by gincommon's ObservabilityMiddlewares; gin itself logs nothing.
func QuietGin() {
	gin.SetMode(gin.ReleaseMode)
	gin.DefaultWriter = io.Discard
	gin.DefaultErrorWriter = io.Discard
}
