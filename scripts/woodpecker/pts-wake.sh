#!/usr/bin/env bash
# pts-wake.sh — provision pts-build-vm for woodpecker compilation (#1669).
# Ephemeral pattern: always creates fresh from ci-agent family image so every
# build gets the latest agent binary and toolchain. No image staleness, no
# stale agent.conf, no manual taint/recreate cycle.
#
# Zone fallback (#150): tries each of the 11 agent zones in sequence until one
# has e2-standard-8 capacity. Zone used is stored in VM metadata so
# pts-cleanup.sh can delete the VM in the correct zone.
#
# States:
#   NOT_FOUND / TERMINATED → create fresh (zone fallback)
#   RUNNING + active owner  → abort (concurrent build in progress)
#   RUNNING + stale owner   → delete + recreate
#
# Build cache lives in GCS — no persistent disk needed.
set -euo pipefail

PTS_BUILD_PROJECT="ci-runners-de"
PTS_BUILD_VM="pts-build-vm"
WP_API="https://d3ci42.peregrinetechsys.net"

# Same zone list used by ci-image-builder bootstrap.sh (peregrine-infrastructure PR #1665)
ZONE_LIST="us-central1-a us-central1-b us-east1-b us-east1-c us-east1-d us-west1-a us-west1-b us-west1-c us-east4-a us-east4-b us-south1-b"

# ── Concurrent-run guard ──
# Describe without --zone so we find the VM regardless of which zone it's in.
DESCRIBE=$(gcloud compute instances list \
    --project="${PTS_BUILD_PROJECT}" \
    --filter="name=${PTS_BUILD_VM}" \
    --format="value(status,zone)" 2>/dev/null | head -1)
STATUS=$(echo "${DESCRIBE}" | awk '{print $1}')
CURRENT_ZONE=$(echo "${DESCRIBE}" | awk '{print $2}' | awk -F/ '{print $NF}')
STATUS="${STATUS:-NOT_FOUND}"

if [ "${STATUS}" = "RUNNING" ] && [ -n "${CURRENT_ZONE}" ]; then
    OWNER_PIPELINE=$(gcloud compute instances describe "${PTS_BUILD_VM}" \
        --zone="${CURRENT_ZONE}" --project="${PTS_BUILD_PROJECT}" \
        --format="value(metadata.items[pts-build-pipeline])" 2>/dev/null || echo "")
    if [ -n "${OWNER_PIPELINE}" ] && [ "${OWNER_PIPELINE}" != "0" ]; then
        OWNER_STATUS=$(curl -s --max-time 5 \
            -H "Authorization: Bearer ${WOODPECKER_API_TOKEN}" \
            "${WP_API}/api/repos/13/pipelines/${OWNER_PIPELINE}" \
            2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin).get('status','unknown'))" \
            2>/dev/null || echo "unknown")
        echo "==> ${PTS_BUILD_VM} RUNNING in ${CURRENT_ZONE} — owner=#${OWNER_PIPELINE} status=${OWNER_STATUS}"
        if [ "${OWNER_STATUS}" = "running" ] || [ "${OWNER_STATUS}" = "pending" ]; then
            echo "    Active pipeline #${OWNER_PIPELINE} is compiling — aborting (#120)."
            exit 0
        fi
    fi
    echo "==> Stale or unowned VM in ${CURRENT_ZONE} — deleting before recreate..."
    gcloud compute instances delete "${PTS_BUILD_VM}" \
        --zone="${CURRENT_ZONE}" --project="${PTS_BUILD_PROJECT}" --quiet 2>/dev/null || true
fi

# ── Create fresh from latest ci-agent family image — zone fallback (#150) ──
echo "==> Creating ${PTS_BUILD_VM} from ci-agent family (pts.${CI_PIPELINE_NUMBER:-0})..."
CREATED_ZONE=""
for ZONE in ${ZONE_LIST}; do
    echo "    Trying zone ${ZONE}..."
    if gcloud compute instances create "${PTS_BUILD_VM}" \
            --project="${PTS_BUILD_PROJECT}" \
            --zone="${ZONE}" \
            --machine-type=e2-standard-8 \
            --image-family=ci-agent \
            --image-project="${PTS_BUILD_PROJECT}" \
            --boot-disk-size=50GB \
            --boot-disk-type=pd-ssd \
            --service-account="ci-agent@${PTS_BUILD_PROJECT}.iam.gserviceaccount.com" \
            --scopes=cloud-platform \
            --tags=pts-build,woodpecker-agent \
            --metadata="agent-label=pts-build,ci-tier=ondemand,pts-build-pipeline=${CI_PIPELINE_NUMBER:-0},pts-build-zone=${ZONE}" \
            --no-address \
            --no-restart-on-failure \
            --maintenance-policy=MIGRATE \
            --quiet 2>&1; then
        CREATED_ZONE="${ZONE}"
        echo "    VM created in ${CREATED_ZONE} from latest ci-agent image"
        break
    fi
    echo "    WARN: ${ZONE} capacity unavailable — trying next zone"
done

if [ -z "${CREATED_ZONE}" ]; then
    echo "ERROR: could not create ${PTS_BUILD_VM} in any zone — all zones at capacity"
    exit 1
fi

# ── Poll until agent registers ──
# woodpecker-agent-config.service runs on boot, writes agent.env, then
# woodpecker-agent.service starts — allow ~3 min for this sequence.
echo "==> Waiting for pts-build-vm agent to register with Woodpecker..."
for i in $(seq 1 36); do
    FOUND=$(curl -sf --max-time 5 "${WP_API}/api/agents" \
        -H "Authorization: Bearer ${WOODPECKER_API_TOKEN}" 2>/dev/null | \
        python3 -c "
import sys,json,time
agents=json.load(sys.stdin)
now=int(time.time())
found=[a for a in agents if a.get('name','').startswith('pts-build-vm') and now-a.get('last_contact',0)<60]
print(len(found))
" 2>/dev/null || echo 0)
    if [ "${FOUND:-0}" -gt 0 ]; then
        echo "    pts-build-vm agent registered (attempt ${i})"
        exit 0
    fi
    echo "    waiting... (attempt ${i}/36, $((i*5))s elapsed)"
    sleep 5
done
echo "ERROR: pts-build-vm agent did not register within 180s"
exit 1
