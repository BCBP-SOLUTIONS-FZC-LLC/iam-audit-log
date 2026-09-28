#!/usr/bin/env bash
# AL-INV-10 / AL-EVT-1 CI gate (LLD §7.3): api/asyncapi.yaml declares only
# `receive` operations — zero `send` — and no message carries the
# `published` tag.
set -euo pipefail

SPEC="api/asyncapi.yaml"
[ -f "$SPEC" ] || { echo "::error file=$SPEC::missing"; exit 1; }

python3 - "$SPEC" <<'PY'
import sys, yaml
spec = yaml.safe_load(open(sys.argv[1]))
errors = []
if not str(spec.get("asyncapi", "")).startswith("3."):
    errors.append("asyncapi must be a 3.x document")
ops = spec.get("operations") or {}
for name, op in ops.items():
    if (op or {}).get("action") != "receive":
        errors.append(f"operation {name!r} has action {op.get('action')!r}; only 'receive' is allowed (AL-EVT-1)")
for name, msg in ((spec.get("components") or {}).get("messages") or {}).items():
    for t in (msg or {}).get("tags") or []:
        if (t.get("name") if isinstance(t, dict) else None) == "published":
            errors.append(f"message {name!r} carries the 'published' tag")
if errors:
    for e in errors:
        print(f"::error file={sys.argv[1]}::{e}")
    sys.exit(1)
print(f"asyncapi receive-only check passed — {len(ops)} receive operation(s), 0 send.")
PY
