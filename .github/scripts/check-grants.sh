#!/usr/bin/env bash
# AL-INV-1 CI gate (LLD §2.4, §4.3, §10.4): the audit_app role is granted
# exactly INSERT + SELECT on audit_events — never UPDATE, DELETE, TRUNCATE,
# ALL, or anything else — and never BYPASSRLS.
#
# Static half (this script): parses every *.up.sql migration. The live half
# (test/postgres grants test, Phase 1) asserts the same set against
# information_schema.role_table_grants on a real migrated database.
set -euo pipefail

MIG_DIR="internal/adapter/outbound/postgres/migrations"
if ! ls "$MIG_DIR"/*.up.sql >/dev/null 2>&1; then
  echo "check-grants: no migrations yet under $MIG_DIR — nothing to assert."
  exit 0
fi

python3 - "$MIG_DIR" <<'PY'
import glob, re, sys

mig_dir = sys.argv[1]
sql = ""
for f in sorted(glob.glob(f"{mig_dir}/*.up.sql")):
    sql += "\n" + open(f).read()
# Strip -- comments.
sql = re.sub(r"--[^\n]*", "", sql)
flat = re.sub(r"\s+", " ", sql)

errors = []

# Every GRANT ... ON [TABLE] [public.]audit_events ... TO ... audit_app.
grant_re = re.compile(
    r"GRANT\s+(?P<privs>[A-Z ,]+?)\s+ON\s+(?:TABLE\s+)?(?P<objs>[\w.,\s]+?)\s+TO\s+(?P<roles>[\w,\s\"]+?)(?:;|\s+WITH\b)",
    re.I,
)
granted = set()
for m in grant_re.finditer(flat):
    objs = {o.strip().split(".")[-1].lower() for o in m.group("objs").split(",")}
    roles = {r.strip().strip('"').lower() for r in m.group("roles").split(",")}
    if "audit_events" in objs and "audit_app" in roles:
        privs = {p.strip().upper() for p in m.group("privs").split(",")}
        granted |= privs

# GRANT ... ON ALL TABLES IN SCHEMA ... TO audit_app would silently cover
# audit_events — forbidden outright.
if re.search(r"GRANT\s+[A-Z ,]+\s+ON\s+ALL\s+TABLES\s+IN\s+SCHEMA\s+\w+\s+TO\s+[\w,\s\"]*\baudit_app\b", flat, re.I):
    errors.append("GRANT ... ON ALL TABLES IN SCHEMA ... TO audit_app is forbidden (would cover audit_events).")
if re.search(r"ALTER\s+DEFAULT\s+PRIVILEGES[^;]*\bTO\s+[\w,\s\"]*\baudit_app\b", flat, re.I):
    errors.append("ALTER DEFAULT PRIVILEGES ... TO audit_app is forbidden (would cover new partitions).")
if re.search(r"\baudit_app\b[^;]*\bBYPASSRLS\b", flat, re.I) and not re.search(r"\baudit_app\b[^;]*\bNOBYPASSRLS\b", flat, re.I):
    errors.append("audit_app must never be BYPASSRLS (AL-INV-3).")

if granted and granted != {"INSERT", "SELECT"}:
    errors.append(f"audit_app on audit_events is granted {sorted(granted)}; must be exactly ['INSERT', 'SELECT'] (AL-INV-1).")
if not granted and re.search(r"\bCREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?(?:public\.)?audit_events\b", flat, re.I) and re.search(r"\baudit_app\b", flat, re.I):
    errors.append("audit_events and audit_app both exist but no INSERT, SELECT grant was found.")

if errors:
    for e in errors:
        print(f"::error::{e}")
    sys.exit(1)
print(f"check-grants passed — audit_app on audit_events: {sorted(granted) or 'none yet'}")
PY
