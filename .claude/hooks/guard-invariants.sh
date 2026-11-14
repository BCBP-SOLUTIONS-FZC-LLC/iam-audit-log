#!/usr/bin/env bash
# PreToolUse guard (Edit|Write|MultiEdit): blocks edits that CI would reject
# later. Exit 2 = block; stderr is fed back to Claude as the reason.
#   1. .env — local secrets/config; edit .env-example instead.
#   2. MIG-1 — a migration already on origin/main is never edited; schema
#      changes go in a new, additive NNNNNN_<name>.{up,down}.sql.
#   3. AL-INV-3 — a non-LOCAL SET app.tenant_id leaks tenant across PgBouncer.
#   4. check-database-access — database/sql, pgxpool.New*/Connect,
#      pgx.Connect in production Go.
#   5. AL-INV-10 — no outbox/publisher wiring; SQS SDK only in
#      cmd/server/main.go for DLQ forwarding (WithDLQForwarding).
#   6. Observability confinement — no raw OTel API or promhttp outside the
#      telemetry adapter (gap 43).
#   7. arch-lint — internal/core/* importing an adapter.
# Only the text being written is checked (Write content, Edit new_string).
# Fails closed: any unexpected error (missing jq, bad JSON) blocks the edit.
# Best-effort only — file changes made via Bash are not seen; CI is the gate.
set -euo pipefail
trap 'echo "guard-invariants.sh failed unexpectedly (line $LINENO); edit blocked. Install jq or fix the hook." >&2; exit 2' ERR

command -v jq >/dev/null || { echo "guard-invariants.sh needs jq (brew install jq); edit blocked." >&2; exit 2; }

input=$(cat)
file=$(jq -r '.tool_input.file_path // empty' <<<"$input")
[ -z "$file" ] && exit 0

# Normalise: absolute, symlinks resolved on the existing parent dir (the file
# itself may not exist yet), so /var vs /private/var and relative paths match.
case "$file" in /*) ;; *) file="${CLAUDE_PROJECT_DIR:-$PWD}/$file" ;; esac
dir=$(dirname "$file")
while [ ! -d "$dir" ]; do dir=$(dirname "$dir"); done
file="$(cd "$dir" && pwd -P)${file#"$dir"}"
path="/${file#/}"
base=$(basename "$path")

content=$(jq -r '[.tool_input.content, .tool_input.new_string, (.tool_input.edits // [] | .[].new_string)] | map(select(. != null)) | join("\n")' <<<"$input")

block() { echo "Blocked ($1): $2" >&2; exit 2; }

# 1. .env
[[ "$base" == ".env" ]] && block ".env" ".env holds local config/secrets and is not edited by Claude. Edit .env-example instead, or ask the user to change .env."

# 2. MIG-1: a migration that exists on origin/main is immutable.
if [[ "$path" == */internal/adapter/outbound/postgres/migrations/*.sql ]]; then
  if top=$(git -C "$dir" rev-parse --show-toplevel 2>/dev/null); then
    top=$(cd "$top" && pwd -P)
    rel="${file#"$top"/}"
    if git -C "$top" cat-file -e "origin/main:$rel" 2>/dev/null; then
      block "MIG-1" "$base is already on main; migrations are additive only. Add a new NNNNNN_<name>.{up,down}.sql (next number after the highest in the directory) instead of changing it."
    fi
  fi
fi

is_prod_go=false
if [[ "$path" == *.go && "$path" != *_test.go && ( "$path" == */cmd/* || "$path" == */internal/* || "$path" == */pkg/* ) && "$path" != */test/* ]]; then
  is_prod_go=true
fi

# 3. AL-INV-3: non-LOCAL SET app.tenant_id leaks tenant across PgBouncer backends.
if [[ ( "$path" == *.go || "$path" == *.sql ) && ( "$path" == */cmd/* || "$path" == */internal/* ) ]] \
  && grep -E '\bSET[[:space:]]+("?app\.tenant_id"?)[[:space:]]*=' <<<"$content" | grep -v 'SET LOCAL' | grep -qv 'set_config'; then
  block "AL-INV-3" "a session-level SET app.tenant_id leaks the tenant across PgBouncer backends. Use pgcommon.WithGUCSet inside RunInTx (SET LOCAL only)."
fi

if $is_prod_go; then
  # 4. check-database-access — no raw DB connections
  if grep -q '"database/sql"' <<<"$content"; then
    block "check-database-access" "database/sql bypasses the pgcommon pool, GUC binding and tracing. Use pgx through pgcommon (internal/adapter/outbound/postgres)."
  fi
  if grep -Eq '\bpgxpool\.(New|NewWithConfig|Connect)[[:space:]]*\(|\bpgx\.Connect(Config)?[[:space:]]*\(' <<<"$content"; then
    block "check-database-access" "construct pools only via pgcommon.NewPool (internal/adapter/outbound/postgres/db.go)."
  fi

  # 5. AL-INV-10: no outbox/publisher wiring
  if grep -Eq 'NewSNSPublisher|LoadSNS\(|LoadOutbox\(|outbox\.Enqueue' <<<"$content"; then
    block "AL-INV-10" "iam-audit-log publishes nothing (AL-INV-10). No SNS publisher, outbox wiring, or Enqueue calls. The only SQS write is DLQ forwarding in cmd/server/main.go."
  fi
  # AL-INV-10: SQS/SNS SDK only in cmd/server/main.go
  if [[ "$path" != */cmd/server/main.go ]] \
    && grep -Eq '"github\.com/aws/aws-sdk-go-v2/service/(sns|sqs)(/[a-z0-9]+)*"' <<<"$content"; then
    block "AL-INV-10" "only cmd/server/main.go may import the SNS/SQS SDK (DLQ forwarding only). Consume via platform-events handlers; there is no publisher."
  fi

  # 6. Observability confinement (gap 43): OTel API and promhttp only in telemetry adapter
  if [[ "$path" != */internal/adapter/outbound/telemetry/* ]]; then
    if grep -Eq '"go\.opentelemetry\.io/otel"' <<<"$content"; then
      block "observability-confinement" "OpenTelemetry API imported outside the telemetry adapter. Use gincommon's NewSpanTracer / SpanTraceID / InitTracingWithConfig."
    fi
    if grep -Eq '"github\.com/prometheus/client_golang/prometheus/promhttp"' <<<"$content"; then
      block "observability-confinement" "promhttp imported outside the telemetry adapter. Serve /metrics via gincommon.MetricsHandler."
    fi
  fi

  # 7. arch-lint: internal/core must not import adapters
  if [[ "$path" == */internal/core/* ]]; then
    hits=$(grep -Eo '"github\.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/adapter/[^"]*"' <<<"$content" || true)
    if [[ "$path" == */internal/core/service/* ]]; then
      hits=$(grep -v '/internal/adapter/outbound/metrics"' <<<"$hits" || true)
    fi
    if [ -n "$hits" ]; then
      block "arch-lint" "internal/core must not import adapters ($(tr '\n' ' ' <<<"$hits")). Add a port interface in internal/core/port and implement it in the adapter."
    fi
  fi
fi

exit 0
