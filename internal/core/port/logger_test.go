package port

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

type entry struct {
	level, msg string
	fields     map[string]any
}

type recLogger struct{ entries []entry }

func (r *recLogger) add(l, m string, f map[string]any) {
	r.entries = append(r.entries, entry{l, m, f})
}
func (r *recLogger) Debug(m string, f map[string]any) { r.add("debug", m, f) }
func (r *recLogger) Info(m string, f map[string]any)  { r.add("info", m, f) }
func (r *recLogger) Warn(m string, f map[string]any)  { r.add("warn", m, f) }
func (r *recLogger) Error(m string, f map[string]any) { r.add("error", m, f) }

func spanCtx(t *testing.T) (context.Context, string) {
	t.Helper()
	tid, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	sid, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled})
	return trace.ContextWithSpanContext(context.Background(), sc), tid.String()
}

func TestSlogStyleLogger_RoutesLevelsAndFields(t *testing.T) {
	rec := &recLogger{}
	l := NewSlogStyleLogger(rec)
	l.Debug("d", "k", 1)
	l.Info("i", "k", 2)
	l.Warn("w", "k", 3)
	l.Error("e", "k", 4)

	want := []string{"debug", "info", "warn", "error"}
	if len(rec.entries) != len(want) {
		t.Fatalf("got %d entries", len(rec.entries))
	}
	for i, e := range rec.entries {
		if e.level != want[i] || e.fields["k"] != i+1 {
			t.Errorf("entry %d = %+v", i, e)
		}
		if _, has := e.fields["trace_id"]; has {
			t.Errorf("non-context variant must not add trace_id")
		}
	}
}

func TestSlogStyleLogger_ContextVariantsAddTraceID(t *testing.T) {
	rec := &recLogger{}
	l := NewSlogStyleLogger(rec)
	ctx, tid := spanCtx(t)
	l.DebugContext(ctx, "d")
	l.InfoContext(ctx, "i")
	l.WarnContext(ctx, "w")
	l.ErrorContext(ctx, "e")
	for _, e := range rec.entries {
		if e.fields["trace_id"] != tid {
			t.Errorf("%s: trace_id = %v, want %s", e.level, e.fields["trace_id"], tid)
		}
	}
	// No span in ctx → no trace_id.
	rec.entries = nil
	l.InfoContext(context.Background(), "x")
	if _, has := rec.entries[0].fields["trace_id"]; has {
		t.Error("trace_id added without a span")
	}
}

func TestSlogStyleLogger_ZeroValueFallsBackToSlog(t *testing.T) {
	var l SlogStyleLogger // nil Logger
	ctx, _ := spanCtx(t)
	l.Info("fallback", "k", "v")
	l.ErrorContext(ctx, "fallback")
}

func TestKVToFields_DropsUnpairedAndNonStringKeys(t *testing.T) {
	f := kvToFields([]any{"a", 1, 2, "x", "trailing"})
	if len(f) != 1 || f["a"] != 1 {
		t.Errorf("fields = %v", f)
	}
}
