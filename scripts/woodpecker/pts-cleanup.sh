#!/usr/bin/env bash
# pts-cleanup.sh — stop pts-build-vm and remove TTL labels.
# Runs on a GCP fleet agent after pts-build-compile completes (success or failure).
# Deploy of woodpecker-server is handled separately by woodpecker-deploy.sh
# via systemd timer — NOT here. Decoupled to avoid the self-kill problem (#74).
set -euo pipefail

PTS_BUILD_PROJECT="ci-runners-de"
PTS_BUILD_ZONE="us-central1-a"
PTS_BUILD_VM="pts-build-vm"

# Mutex guard: the wake step labels the VM with pts-build-pipeline=N.
# If a newer pipeline has already claimed the VM, skip the stop — we'd
# be killing a concurrent wake step mid-boot (#109).
MY_PIPELINE="${CI_PIPELINE_NUMBER:-0}"
OWNER_PIPELINE=$(gcloud compute instances describe "${PTS_BUILD_VM}" \
    --zone="${PTS_BUILD_ZONE}" --project="${PTS_BUILD_PROJECT}" \
    --format="value(labels.pts-build-pipeline)" 2>/dev/null || echo "")
echo "==> Mutex check: my pipeline=#${MY_PIPELINE} owner=#${OWNER_PIPELINE:-unknown}"

if [ -n "${OWNER_PIPELINE}" ] && [ "${OWNER_PIPELINE}" -gt "${MY_PIPELINE}" ] 2>/dev/null; then
    echo "    VM owned by newer pipeline #${OWNER_PIPELINE} — skipping stop (superseded)"
else
    echo "==> Stopping ${PTS_BUILD_VM}..."
    gcloud compute instances stop "${PTS_BUILD_VM}" \
        --zone="${PTS_BUILD_ZONE}" --project="${PTS_BUILD_PROJECT}" --quiet 2>/dev/null && \
        echo "    stopped" || echo "    ⚠️  stop failed or VM already stopped (non-fatal)"

    echo "==> Removing TTL labels..."
    gcloud compute instances remove-labels "${PTS_BUILD_VM}" \
        --zone="${PTS_BUILD_ZONE}" --project="${PTS_BUILD_PROJECT}" \
        --labels=ttl-expire-epoch,ttl-override-min,pts-build-pipeline 2>/dev/null && \
        echo "    labels removed" || echo "    ⚠️  label removal failed (non-fatal)"
fi
