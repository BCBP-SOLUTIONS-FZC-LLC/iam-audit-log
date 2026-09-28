package port

import "context"

// Tracer starts spans on the TracerProvider platform-gincommon installs. It is
// structurally identical to pgcommon.Config.Tracer, and is implemented only
// by the telemetry adapter (BUILD_PLAN gap 43).
type Tracer interface {
	StartSpan(ctx context.Context, name string) (spanCtx context.Context, end func())
}

// TraceIDFunc returns the active span's trace id, or "".
type TraceIDFunc func(ctx context.Context) string
