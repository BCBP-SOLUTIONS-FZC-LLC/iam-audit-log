#!/usr/bin/env bash
# AL-INV-10 companion to check-forbidden-events-bypass.sh: iam-audit-log has
# NO outbox (LLD §2.4, §4.4 — "there is no outbox.ApplySchema step"), so any
# SQL touching outbox_events in production code is a violation. Unlike
# iam-org-membership's version of this script there is deliberately no
# allowed directory: platform-events' outbox is never wired here either.
#
# Scope: internal/, cmd/, pkg/ (Go raw-string SQL literals) and the embedded
# migrations (*.sql). test/ is excluded.
#
# Detection mirrors iam-org-membership: Go backtick literals only (DOTALL,
# so multi-line SQL is one match), so doc comments that *name* the
# forbidden table never false-positive. In .sql files, `--` comments are
# stripped first.
set -euo pipefail

python3 - <<'PYEOF'
import re
import sys
from pathlib import Path

ROOTS = ["internal", "cmd", "pkg"]
SQL_RE = re.compile(r'\b(from|into|update|table|join)\s+(?:if\s+(?:not\s+)?exists\s+)?(?:public\.)?outbox_events\b', re.IGNORECASE)
BACKTICK_RE = re.compile(r'`([^`]*)`', re.DOTALL)

violations = []
for root in ROOTS:
    for path in Path(root).rglob("*.go"):
        posix = path.as_posix()
        if posix.endswith("_test.go"):
            continue
        text = path.read_text(encoding="utf-8")
        for m in BACKTICK_RE.finditer(text):
            if SQL_RE.search(m.group(1)):
                violations.append(f"{posix}:{text.count(chr(10), 0, m.start()) + 1}")
    for path in Path(root).rglob("*.sql"):
        posix = path.as_posix()
        lines = path.read_text(encoding="utf-8").splitlines()
        for i, line in enumerate(lines, 1):
            if SQL_RE.search(line.split("--", 1)[0]):
                violations.append(f"{posix}:{i}")

if violations:
    for v in violations:
        print(f"::error file={v}::SQL against outbox_events — iam-audit-log has no outbox and publishes no events (AL-INV-10)")
    print(f"\n{len(violations)} violation(s). See .github/scripts/check-outbox-access.sh for rationale.")
    sys.exit(1)

print("  ✔  no SQL against outbox_events (AL-INV-10: this service has no outbox)")
PYEOF
