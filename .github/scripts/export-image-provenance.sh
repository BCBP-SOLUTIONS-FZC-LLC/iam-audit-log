#!/usr/bin/env bash
# Extracts the BuildKit-native SLSA provenance attestation from a pushed
# image and writes provenance.slsa.json.
#
# Uses `docker buildx imagetools inspect`, NOT `cosign download attestation`.
# docker/build-push-action's `provenance`/`sbom` inputs create BuildKit-native
# attestations (manifests in the image's own index), a different format from
# the attestations `cosign attest` creates, which is all `cosign download
# attestation` can find. The v0.1.0 release run failed on exactly that
# ("found no attestations"). Modeled on iam-authz-enrichment's script.
set -euo pipefail

: "${IMAGE_NAME:?IMAGE_NAME is required}"
: "${IMAGE_DIGEST:?IMAGE_DIGEST is required}"

IMAGE_REF="${IMAGE_NAME}@${IMAGE_DIGEST}"

PROVENANCE=$(docker buildx imagetools inspect "${IMAGE_REF}" --format '{{ json .Provenance.SLSA }}')

if [ -z "${PROVENANCE}" ] || [ "${PROVENANCE}" = "null" ]; then
  echo "::error title=Provenance export::SLSA provenance attestation not found on ${IMAGE_REF}"
  exit 1
fi

echo "${PROVENANCE}" | jq . > provenance.slsa.json
echo "  ✔  exported SLSA provenance to provenance.slsa.json"
