#!/usr/bin/env bash
# pts-cleanup.sh — stop pentest-dev-vm and remove TTL label.
# Runs on d3ci42-local (backend: local). Always runs — success or failure.
set -euo pipefail

PENTEST_PROJECT="peregrine-pentest-dev"
PENTEST_ZONE="us-central1-a"
PENTEST_VM="pentest-dev-vm"

echo "==> Stopping ${PENTEST_VM}..."
gcloud compute instances stop "${PENTEST_VM}" \
    --zone="${PENTEST_ZONE}" --project="${PENTEST_PROJECT}" --quiet 2>/dev/null && \
    echo "    stopped" || echo "    ⚠️  stop failed or VM already stopped (non-fatal)"

echo "==> Removing TTL labels..."
gcloud compute instances remove-labels "${PENTEST_VM}" \
    --zone="${PENTEST_ZONE}" --project="${PENTEST_PROJECT}" \
    --labels=ttl-expire-epoch,pts-build-pipeline 2>/dev/null && \
    echo "    labels removed" || echo "    ⚠️  label removal failed (non-fatal)"
