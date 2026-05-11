#!/usr/bin/env bash
# pts-promote.sh — write the pending-deploy marker to GCS.
# Runs as a local step on d3ci42 AFTER pts-build-cleanup completes (#219).
# By deferring the pending marker to this step, the server restart triggered
# by woodpecker-deploy.sh happens only after the full pipeline is done —
# cleanup is never killed mid-run by the server it just helped deploy.
set -euo pipefail

VERSION="v3.13.0-pts.${CI_PIPELINE_NUMBER:-0}"
COMMIT_SHA="${CI_COMMIT_SHA:-}"
BUILD_CACHE_BUCKET="gs://ci-runners-de-build-cache"
DEPLOY_BUCKET="${BUILD_CACHE_BUCKET}/woodpecker-deploy"

echo "==> pts-promote: ${VERSION}"

# Confirm the versioned binary exists before writing the marker — guards
# against a promote step running after a failed compile that never uploaded.
if ! gsutil -q stat "${DEPLOY_BUCKET}/${VERSION}/woodpecker-server" 2>/dev/null; then
    echo "    ❌ Binary not found at ${DEPLOY_BUCKET}/${VERSION}/woodpecker-server — aborting promote"
    exit 1
fi

printf '%s\n%s\n%s' "${VERSION}" "${COMMIT_SHA}" "${CI_PIPELINE_NUMBER:-0}" | \
    gsutil -q cp - "${DEPLOY_BUCKET}/pending"

echo "    Pending marker written — woodpecker-deploy.sh will pick up within 30s"
echo "    Server restart will complete deployment of ${VERSION}"
