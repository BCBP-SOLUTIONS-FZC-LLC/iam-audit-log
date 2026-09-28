package telemetry

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/trace"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

func TestTracer_StartSpan(t *testing.T) {
	for _, name := range []string{"", "iam-audit-log"} {
		ctx, end := NewTracer(name).StartSpan(context.Background(), "q")
		if ctx == nil {
			t.Fatal("nil ctx")
		}
		end()
	}
}

func TestTraceID(t *testing.T) {
	if got := TraceID(context.Background()); got != "" {
		t.Errorf("no span: %q", got)
	}
	tid, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	sid, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	ctx := trace.ContextWithSpanContext(context.Background(),
		trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled}))
	if got := TraceID(ctx); got != tid.String() {
		t.Errorf("TraceID = %q, want %s", got, tid)
	}
}

// /metrics serves exactly gincommon's registry: a collector registered on
// gincommon.MetricsRegisterer() appears in the scrape.
func TestMetricsHandler_ServesGincommonRegistry(t *testing.T) {
	_ = gincommon.ObservabilityMiddlewares(gincommon.Config{ServiceName: "iam-audit-log", BuildVersion: "test"})
	c := prometheus.NewCounter(prometheus.CounterOpts{Name: "iam_audit_log_telemetry_probe_total", Help: "test"})
	if err := gincommon.MetricsRegisterer().Register(c); err != nil {
		t.Fatal(err)
	}
	c.Inc()
	rec := httptest.NewRecorder()
	MetricsHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)
	if rec.Code != 200 || !strings.Contains(string(body), "iam_audit_log_telemetry_probe_total 1") {
		t.Fatalf("status %d, body missing the probe:\n%.500s", rec.Code, body)
	}
}

type recLog struct{ errs []map[string]any }

func (r *recLog) Debug(string, map[string]any)     {}
func (r *recLog) Info(string, map[string]any)      {}
func (r *recLog) Warn(string, map[string]any)      {}
func (r *recLog) Error(_ string, f map[string]any) { r.errs = append(r.errs, f) }

// net/http's own error lines land in the gincommon logger, not stderr.
func TestHTTPErrorLog_ForwardsToLogger(t *testing.T) {
	rec := &recLog{}
	HTTPErrorLog(rec, "api").Printf("http: TLS handshake error from %s", "10.0.0.1:1234")
	if len(rec.errs) != 1 || rec.errs[0]["server"] != "api" ||
		rec.errs[0]["error"] != "http: TLS handshake error from 10.0.0.1:1234" {
		t.Fatalf("forwarded = %v", rec.errs)
	}
	HTTPErrorLog(nil, "api").Print("dropped, no panic")
}

func TestQuietGin(t *testing.T) {
	QuietGin()
	if gin.Mode() != gin.ReleaseMode || gin.DefaultWriter != io.Discard || gin.DefaultErrorWriter != io.Discard {
		t.Fatalf("gin mode=%s writers not discarded", gin.Mode())
	}
}
