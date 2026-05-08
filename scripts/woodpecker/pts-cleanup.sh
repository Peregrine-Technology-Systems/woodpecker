#!/usr/bin/env bash
# pts-cleanup.sh — delete pts-build-vm after compile completes.
# Ephemeral pattern (#1669): VM is deleted so next build always creates fresh
# from latest ci-agent family image. Build cache persists in GCS.
# Zone is read from VM metadata (pts-build-zone) since zone fallback (#150)
# may have created the VM in any of the 11 agent zones.
set -euo pipefail

PTS_BUILD_PROJECT="ci-runners-de"
PTS_BUILD_VM="pts-build-vm"
MY_PIPELINE="${CI_PIPELINE_NUMBER:-0}"

# Find VM and its zone regardless of which zone it landed in
DESCRIBE=$(gcloud compute instances list \
    --project="${PTS_BUILD_PROJECT}" \
    --filter="name=${PTS_BUILD_VM}" \
    --format="value(zone,metadata.items[pts-build-pipeline])" 2>/dev/null | head -1)
PTS_BUILD_ZONE=$(echo "${DESCRIBE}" | awk '{print $1}' | awk -F/ '{print $NF}')
OWNER=$(echo "${DESCRIBE}" | awk '{print $2}')

if [ -z "${PTS_BUILD_ZONE}" ]; then
    echo "==> ${PTS_BUILD_VM} not found — already deleted or never created"
    exit 0
fi

echo "==> Mutex check: my pipeline=#${MY_PIPELINE} owner=#${OWNER:-unknown} zone=${PTS_BUILD_ZONE}"

if [ -n "${OWNER}" ] && [ "${OWNER}" -gt "${MY_PIPELINE}" ] 2>/dev/null; then
    echo "    VM owned by newer pipeline #${OWNER} — skipping delete (superseded)"
    exit 0
fi

echo "==> Deleting ${PTS_BUILD_VM} in ${PTS_BUILD_ZONE} (ephemeral — fresh image on next build)..."
gcloud compute instances delete "${PTS_BUILD_VM}" \
    --zone="${PTS_BUILD_ZONE}" --project="${PTS_BUILD_PROJECT}" --quiet 2>/dev/null && \
    echo "    deleted" || echo "    ⚠️  delete failed or VM already gone (non-fatal)"
