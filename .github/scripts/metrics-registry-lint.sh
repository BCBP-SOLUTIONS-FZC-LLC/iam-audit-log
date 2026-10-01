#!/usr/bin/env bash
# Enforces the Enterprise Platform Observability Standard's governance
# rules 11/12 for Proposed (submitted, not yet ratified) platform_* metrics.
# internal/adapter/outbound/metrics/registry.go records every platform_*
# metric this repo's monitoring reads, with its status mirrored from the
# central Platform Observability Registry
# (TestRegistryLedger_MatchesPlatformRegistry keeps the two in step). The
# platform libraries removed every pre-standard name and several successors
# are only Proposed, so using them is unavoidable; every live use must be
# marked:
#
#   - an alert whose expr reads a Proposed metric, or a recording rule built
#     on one, carries the label `metric_status: proposed`;
#   - such a recording rule has a `# metric_status: proposed` comment on the
#     line above its `- record:`;
#   - a dashboard panel querying one has "(Proposed)" in its title;
#   - a prometheus-adapter rule on one has a `# metric_status: proposed`
#     comment above its `- seriesQuery:`, and the HPA carries the
#     `observability.platform/metric-status: proposed` annotation.
#
# A mention in a comment is fine. When governance ratifies a metric, flip its
# Status to StatusCanonical in registry.go and drop the markers.
# deploy/helm/templates/prometheusrule.yaml is generated from the static rule
# files (TestAlerts_StaticMirrorMatchesHelm), so those are what is checked.
set -euo pipefail

cd "$(dirname "$0")/../.."

python3 - <<'PYEOF'
import json
import re
import sys

try:
    import yaml
except ImportError:
    print("::error::metrics-registry-lint.sh needs PyYAML (python3 -m pip install pyyaml)")
    sys.exit(1)

REGISTRY_GO = "internal/adapter/outbound/metrics/registry.go"
RULE_FILES = [
    "deploy/monitoring/app-alerts.yml",
    "deploy/monitoring/recording-rules.yml",
    "deploy/monitoring/slo-rules.yml",
]
DASHBOARD = "deploy/monitoring/dashboard-audit-log.json"
ADAPTER = "deploy/monitoring/prometheus-adapter-rule.yaml"
HPA = "deploy/helm/templates/hpa.yaml"
MARK = "metric_status: proposed"

# registry.go: Name:/Status: on separate lines (gofmt-aligned struct literals).
proposed, name = set(), None
for line in open(REGISTRY_GO, encoding="utf-8"):
    m = re.search(r'Name:\s*"([^"]+)"', line)
    if m:
        name = m.group(1)
        continue
    if "Status:" in line and name:
        if "StatusProposed" in line:
            proposed.add(name)
        name = None
if not proposed:
    print(f"OK: no Proposed platform_* metrics in {REGISTRY_GO}")
    sys.exit(0)
print("Proposed (unratified) platform_* metrics per registry.go:")
for n in sorted(proposed):
    print(f"  - {n}")

ident = re.compile(r"[a-zA-Z_:][a-zA-Z0-9_:]*")


def reads(expr, names):
    """The Proposed names (or Proposed-derived recording rules) expr reads."""
    hits = set()
    for tok in ident.findall(expr or ""):
        base = re.sub(r"_(bucket|count|sum)$", "", tok)
        if base in names:
            hits.add(base)
        elif tok in names:
            hits.add(tok)
    return hits


errors = []
rules = []
for f in RULE_FILES:
    text = open(f, encoding="utf-8").read()
    for g in (yaml.safe_load(text) or {}).get("groups", []):
        for r in g.get("rules", []):
            rules.append((f, text.splitlines(), r))

# Recording rules built (transitively) on a Proposed metric are Proposed too.
derived = set(proposed)
changed = True
while changed:
    changed = False
    for _, _, r in rules:
        if "record" in r and r["record"] not in derived and reads(r.get("expr"), derived):
            derived.add(r["record"])
            changed = True

for f, lines, r in rules:
    hits = reads(r.get("expr"), derived)
    if not hits:
        continue
    if "record" in r:
        pat = re.compile(r"\s*- record:\s*" + re.escape(r["record"]) + r"\s*$")
        idx = next((i for i, l in enumerate(lines) if pat.match(l)), None)
        if idx is None or idx == 0 or MARK not in lines[idx - 1]:
            errors.append(f"{f}: recording rule {r['record']} reads Proposed {sorted(hits)} without a '# {MARK}' comment above it")
    elif (r.get("labels") or {}).get("metric_status") != "proposed":
        errors.append(f"{f}: alert {r.get('alert')} reads Proposed {sorted(hits)} without the label {MARK}")

dash = json.load(open(DASHBOARD, encoding="utf-8"))
for p in dash.get("panels", []):
    hits = set()
    for t in p.get("targets", []):
        hits |= reads(t.get("expr"), derived)
    if hits and "(Proposed)" not in p.get("title", ""):
        errors.append(f"{DASHBOARD}: panel {p.get('title')!r} queries Proposed {sorted(hits)}; add \"(Proposed)\" to its title")

adapter_lines = open(ADAPTER, encoding="utf-8").read().splitlines()
adapter_proposed = False
for i, l in enumerate(adapter_lines):
    m = re.match(r"\s*- seriesQuery:\s*'([^']*)'", l)
    if not m or not reads(m.group(1), derived):
        continue
    adapter_proposed = True
    if i == 0 or MARK not in adapter_lines[i - 1]:
        errors.append(f"{ADAPTER}:{i + 1}: adapter rule on a Proposed metric without a '# {MARK}' comment above it")
if adapter_proposed and "observability.platform/metric-status: proposed" not in open(HPA, encoding="utf-8").read():
    errors.append(f"{HPA}: scales on Proposed metrics (via {ADAPTER}) without the observability.platform/metric-status: proposed annotation")

if errors:
    print()
    for e in errors:
        print(f"::error::{e}")
    print(f"\n{len(errors)} unmarked use(s) of Proposed platform_* metrics (Enterprise Platform Observability Standard, rules 11/12).")
    sys.exit(1)
print(f"\nOK: every use of a Proposed platform_* metric is marked {MARK!r}")
PYEOF
