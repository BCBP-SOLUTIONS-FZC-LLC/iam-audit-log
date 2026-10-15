#!/usr/bin/env bash
# Stage produced event schemas for Glue Schema Registry registration.
#
# Copies schema files from internal/eventschema/ to the staging directory,
# renaming them from snake_case to PascalCase Glue names where applicable.
#
# NOTE: iam-audit-log is a PURE CONSUMER (AL-INV-10). It produces no events
# and registers no schemas in Glue. This script is a no-op by design but is
# present so the schema-registry.yml workflow structure matches the platform
# standard without needing service-specific workflow branches.
#
# Usage: bash .github/scripts/stage-produced-event-schemas.sh <output-dir>
set -euo pipefail

OUTPUT_DIR="${1:?Usage: stage-produced-event-schemas.sh <output-dir>}"

mkdir -p "${OUTPUT_DIR}"

# This service produces no schemas — the output directory will be empty.
# schema-gov register/diff will be no-ops (nothing to register).
echo "iam-audit-log is a pure consumer service (AL-INV-10): no produced schemas to stage."
echo "Output directory: ${OUTPUT_DIR} (empty — this is correct by design)"

# Emit step summary so CI logs are transparent about the no-op.
if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  echo "### Schema staging: no produced schemas" >> "${GITHUB_STEP_SUMMARY}"
  echo "iam-audit-log consumes events but does not publish any (AL-INV-10). No schemas staged." >> "${GITHUB_STEP_SUMMARY}"
fi
