#!/usr/bin/env bash
# Runs platform-gincommon's metricslint (the Enterprise Platform Observability
# Standard's own linter, version pinned by go.mod) against this service.
#
#   1. check: a real /metrics scrape in the production registration order
#      (test/unit TestMetrics_ScrapeValidatesAgainstRegistry, which records one
#      sample on every collector this service owns, so every label set is
#      validated, and adds platform-events' and platform-pgcommon's collectors).
#   2. refs:  alert rules, recording rules, SLOs, the dashboard, the HPA
#      adapter rules and the operator Markdown — legacy (removed) or
#      unregistered platform_* names.
#   3. inventory drift: docs/observability/metric-registry.md must match
#      what .github/scripts/metrics-inventory.py renders from the registry
#      and the scrape. METRICS_INVENTORY_WRITE=1 rewrites it instead
#      (`make metrics-inventory`).
#
set -euo pipefail

ALLOW_PROPOSED_OWNERS="platform-gincommon,platform-events,platform-pgcommon"
GINCOMMON=github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

go build -o "$work/metricslint" "$GINCOMMON/cmd/metricslint"

METRICS_SCRAPE_OUT="$work/scrape.txt" \
  go test ./test/unit/ -count=1 -run '^TestMetrics_ScrapeValidatesAgainstRegistry$' >/dev/null

set +e
"$work/metricslint" check -allow-proposed-owner "$ALLOW_PROPOSED_OWNERS" "$work/scrape.txt" >"$work/check.txt" 2>&1
check_rc=$?
set -e
cat "$work/check.txt"
if [ "$check_rc" -gt 1 ]; then
  echo "metricslint check: usage or I/O error (exit $check_rc)" >&2
  exit "$check_rc"
fi
if [ "$check_rc" -ne 0 ]; then
  echo "::error::metricslint check found violations of the observability standard (see above)."
  exit 1
fi
echo "metricslint check passed."

# refs walks directories for rule/dashboard extensions only (.yml, .json, ...),
# so the operator Markdown is named explicitly (a named file is always
# scanned). migration.md is left out: it lists the removed names on purpose.
"$work/metricslint" refs deploy docs/runbook.md docs/observability/README.md \
  docs/observability/runbooks.md docs/observability/metric-registry.md
echo "metricslint refs passed."

gincommon_dir=$(go list -m -f '{{.Dir}}' "$GINCOMMON")
gincommon_version=$(go list -m -f '{{.Version}}' "$GINCOMMON")
inventory=docs/observability/metric-registry.md
python3 .github/scripts/metrics-inventory.py "$gincommon_dir/internal/obsregistry/registry.json" \
  "$work/scrape.txt" "$gincommon_version" internal/adapter/outbound/metrics/registry.go >"$work/metric-registry.md"
if [ "${METRICS_INVENTORY_WRITE:-}" = "1" ]; then
  cp "$work/metric-registry.md" "$inventory"
  echo "wrote $inventory"
elif ! diff -u "$inventory" "$work/metric-registry.md"; then
  echo "::error file=${inventory}::metric inventory is out of date; run make metrics-inventory"
  exit 1
else
  echo "metric inventory up to date."
fi
