// Package port declares the interfaces the core requires (LLD §3: AuditWriter,
// AuditReader, ArchiveStore, Clock, Ledger, …). It imports core/domain only.
package port

import "context"

// Logger is the structured logging port this service funnels through: HTTP
// middleware (via platform-gincommon), core services, the SQS consumer
// fleet, outbound clients, and the reconciler binary all end up writing
// through the same sink. It matches platform-gincommon's own
// port.Logger shape exactly (Debug/Info/Warn/Error(msg, fields)), so the
// single Zap-backed logger built once via platform-gincommon's
// logger.NewLogger (cmd/server/main.go, cmd/reconciler/main.go) satisfies
// it directly — no adapter needed at the call site that builds it.
type Logger interface {
	Debug(msg string, fields map[string]any)
	Info(msg string, fields map[string]any)
	Warn(msg string, fields map[string]any)
	Error(msg string, fields map[string]any)
}

// SlogStyleLogger adapts a Logger to slog-style call conventions — both the
// plain (msg, "key", val, ...) and *Context (ctx, msg, "key", val, ...)
// variants — while still writing only through the wrapped Logger, which is
// always platform-gincommon's (BUILD_PLAN gap 43). A nil Logger drops lines
// rather than falling back to another sink.
type SlogStyleLogger struct {
	log     Logger
	traceID TraceIDFunc
}

// NewSlogStyleLogger wraps log for slog-style call sites. traceID (may be
// nil) supplies the trace_id field on the *Context variants.
func NewSlogStyleLogger(log Logger, traceID TraceIDFunc) SlogStyleLogger {
	return SlogStyleLogger{log: log, traceID: traceID}
}

type level int

const (
	levelDebug level = iota
	levelInfo
	levelWarn
	levelError
)

// Debug logs at debug level with slog-style alternating key/value args.
func (s SlogStyleLogger) Debug(msg string, args ...any) {
	s.write(context.Background(), false, levelDebug, msg, args)
}

// Info logs at info level with slog-style alternating key/value args.
func (s SlogStyleLogger) Info(msg string, args ...any) {
	s.write(context.Background(), false, levelInfo, msg, args)
}

// Warn logs at warn level with slog-style alternating key/value args.
func (s SlogStyleLogger) Warn(msg string, args ...any) {
	s.write(context.Background(), false, levelWarn, msg, args)
}

// Error logs at error level with slog-style alternating key/value args.
func (s SlogStyleLogger) Error(msg string, args ...any) {
	s.write(context.Background(), false, levelError, msg, args)
}

// DebugContext logs at debug level, adding trace_id from ctx when present.
func (s SlogStyleLogger) DebugContext(ctx context.Context, msg string, args ...any) {
	s.write(ctx, true, levelDebug, msg, args)
}

// InfoContext logs at info level, adding trace_id from ctx when present.
func (s SlogStyleLogger) InfoContext(ctx context.Context, msg string, args ...any) {
	s.write(ctx, true, levelInfo, msg, args)
}

// WarnContext logs at warn level, adding trace_id from ctx when present.
func (s SlogStyleLogger) WarnContext(ctx context.Context, msg string, args ...any) {
	s.write(ctx, true, levelWarn, msg, args)
}

// ErrorContext logs at error level, adding trace_id from ctx when present.
func (s SlogStyleLogger) ErrorContext(ctx context.Context, msg string, args ...any) {
	s.write(ctx, true, levelError, msg, args)
}

func (s SlogStyleLogger) write(ctx context.Context, withTrace bool, lvl level, msg string, args []any) {
	if s.log == nil {
		return
	}
	fields := kvToFields(args)
	if withTrace && s.traceID != nil {
		if id := s.traceID(ctx); id != "" {
			fields["trace_id"] = id
		}
	}
	switch lvl {
	case levelDebug:
		s.log.Debug(msg, fields)
	case levelInfo:
		s.log.Info(msg, fields)
	case levelWarn:
		s.log.Warn(msg, fields)
	default:
		s.log.Error(msg, fields)
	}
}

// kvToFields pairs alternating string-key/value slog-style args into a map.
// A trailing unpaired argument, or one whose key isn't a string, is dropped
// rather than panicking.
func kvToFields(args []any) map[string]any {
	fields := make(map[string]any, len(args)/2)
	for i := 0; i+1 < len(args); i += 2 {
		if key, ok := args[i].(string); ok {
			fields[key] = args[i+1]
		}
	}
	return fields
}
