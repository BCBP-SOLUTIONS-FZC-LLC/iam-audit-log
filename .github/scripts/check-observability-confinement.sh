#!/usr/bin/env bash
# Every log, metric and trace goes through platform-gincommon (BUILD_PLAN gap 43).
#
#   Logs:    only gincommon's logger (pkg/logger.NewLogger). No log/slog, the
#            standard "log" package, zap or fmt.Print* anywhere — except the
#            telemetry adapter's http.Server.ErrorLog bridge, which forwards
#            net/http's own lines into the gincommon logger. Stderr only in a
#            composition root, for the one case where the gincommon logger
#            itself cannot be built.
#   Metrics: collectors register only on gincommon.MetricsRegisterer() — no
#            prometheus.MustRegister/Register/DefaultRegisterer/NewRegistry.
#            /metrics is served only by gincommon.MetricsHandler (through the
#            telemetry adapter); promhttp is imported nowhere.
#   Traces:  the OpenTelemetry API is imported nowhere: spans, trace ids and
#            tracing init come from gincommon's tracing API (NewSpanTracer,
#            SpanTraceID, InitTracingWithConfig; platform-gincommon >= v1.4.0).
#
# Non-test Go under cmd/, internal/, pkg/, api/. test/ is exempt (harnesses).
set -euo pipefail

fail=0
report() { echo "::error::$1"; echo "$2" | sed 's/^/    /'; fail=1; }
src() { grep -rlE "$1" --include='*.go' cmd internal pkg api 2>/dev/null | grep -v '_test\.go$' || true; }

telemetry='^internal/adapter/outbound/telemetry/'

o=$(src '"go\.opentelemetry\.io/' || true)
[ -z "$o" ] || report "OpenTelemetry API imported (use gincommon's tracing API: NewSpanTracer, SpanTraceID, InitTracingWithConfig):" "$o"

o=$(src '"github\.com/prometheus/client_golang/prometheus/promhttp"' || true)
[ -z "$o" ] || report "promhttp imported (serve /metrics via gincommon.MetricsHandler):" "$o"

o=$(src 'prometheus\.(MustRegister|Register|DefaultRegisterer|DefaultGatherer|NewRegistry)\b' | grep -vE "$telemetry" || true)
[ -z "$o" ] || report "collector registered outside gincommon.MetricsRegisterer():" "$o"

o=$(src '"log/slog"|stdlog "log"|^[[:space:]]*"log"[[:space:]]*$|"go\.uber\.org/zap' | grep -vE "$telemetry" || true)
[ -z "$o" ] || report "a logging package other than platform-gincommon's logger is imported:" "$o"

o=$(src '\bfmt\.(Print|Printf|Println)\(|\bprintln\(|\bprint\(' || true)
[ -z "$o" ] || report "fmt.Print*/print writes a log line outside the gincommon logger:" "$o"

o=$(src '\bos\.(Stderr|Stdout)\b' | grep -vE '^cmd/[^/]+/main\.go$' || true)
[ -z "$o" ] || report "os.Stderr/os.Stdout used outside a composition root:" "$o"

for f in cmd/*/main.go; do
  n=$(grep -cE '\bos\.Stderr\b' "$f" || true)
  if [ "$n" -gt 1 ]; then
    report "$f writes to stderr $n times; only the logger-init failure may (use the gincommon logger):" "$(grep -nE '\bos\.Stderr\b' "$f")"
  fi
done

[ "$fail" -eq 0 ] && echo "observability confinement OK — logs, metrics and traces all go through platform-gincommon."
exit "$fail"
