#!/usr/bin/env bash
# pts-cleanup.sh — delete pts-build-vm after compile completes.
# Ephemeral pattern (#1669): VM is deleted so next build always creates fresh
# from latest ci-agent family image. Build cache persists in GCS.
set -euo pipefail

PTS_BUILD_PROJECT="ci-runners-de"
PTS_BUILD_ZONE="us-central1-a"
PTS_BUILD_VM="pts-build-vm"

MY_PIPELINE="${CI_PIPELINE_NUMBER:-0}"

# Mutex guard: skip delete if a newer pipeline has already claimed the VM.
OWNER=$(gcloud compute instances describe "${PTS_BUILD_VM}" \
    --zone="${PTS_BUILD_ZONE}" --project="${PTS_BUILD_PROJECT}" \
    --format="value(metadata.items[pts-build-pipeline])" 2>/dev/null || echo "")
echo "==> Mutex check: my pipeline=#${MY_PIPELINE} owner=#${OWNER:-unknown}"

if [ -n "${OWNER}" ] && [ "${OWNER}" -gt "${MY_PIPELINE}" ] 2>/dev/null; then
    echo "    VM owned by newer pipeline #${OWNER} — skipping delete (superseded)"
    exit 0
fi

echo "==> Deleting ${PTS_BUILD_VM} (ephemeral — fresh image on next build)..."
gcloud compute instances delete "${PTS_BUILD_VM}" \
    --zone="${PTS_BUILD_ZONE}" --project="${PTS_BUILD_PROJECT}" --quiet 2>/dev/null && \
    echo "    deleted" || echo "    ⚠️  delete failed or VM already gone (non-fatal)"
