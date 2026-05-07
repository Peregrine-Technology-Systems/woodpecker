#!/usr/bin/env bash
# pts-wake.sh — start pentest-dev-vm and wait for the Woodpecker agent to register.
# The agent (woodpecker-agent.service) auto-starts via systemd on VM boot.
# No SSH required — just start the VM and poll the Woodpecker API.
set -euo pipefail

PENTEST_PROJECT="peregrine-pentest-dev"
PENTEST_ZONE="us-central1-a"
PENTEST_VM="pentest-dev-vm"
TTL_MINUTES=120  # generous budget: boot + cache restore + compile + deploy

WP_SERVER="d3ci42.peregrinetechsys.net"  # used only for reference

# ── Concurrent-run guard ──
# If pentest-dev-vm is already RUNNING and owned by an ACTIVE pipeline, abort.
CURRENT_STATUS=$(gcloud compute instances describe "${PENTEST_VM}" \
    --zone="${PENTEST_ZONE}" --project="${PENTEST_PROJECT}" \
    --format="value(status)" 2>/dev/null || echo "UNKNOWN")
if [ "${CURRENT_STATUS}" = "RUNNING" ]; then
    OWNER_PIPELINE=$(gcloud compute instances describe "${PENTEST_VM}" \
        --zone="${PENTEST_ZONE}" --project="${PENTEST_PROJECT}" \
        --format="value(labels.pts-build-pipeline)" 2>/dev/null || echo "")
    if [ -n "${OWNER_PIPELINE}" ] && [ "${OWNER_PIPELINE}" != "0" ]; then
        OWNER_STATUS=$(curl -s --max-time 5 \
            -H "Authorization: Bearer ${WOODPECKER_API_TOKEN}" \
            "http://localhost:8000/api/repos/13/pipelines/${OWNER_PIPELINE}" \
            2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin).get('status','unknown'))" \
            2>/dev/null || echo "unknown")
        echo "==> ${PENTEST_VM} RUNNING — owner=#${OWNER_PIPELINE} status=${OWNER_STATUS}"
        if [ "${OWNER_STATUS}" = "running" ] || [ "${OWNER_STATUS}" = "pending" ]; then
            echo "    Active pipeline #${OWNER_PIPELINE} is compiling — skipping (#120)."
            exit 0
        else
            echo "    Stale label from #${OWNER_PIPELINE} (${OWNER_STATUS}) — taking ownership."
        fi
    else
        echo "==> ${PENTEST_VM} RUNNING with no owner label — taking ownership."
    fi
fi

echo "==> Starting ${PENTEST_VM} for pts-build v3.13.0-pts.${CI_PIPELINE_NUMBER:-0}..."
gcloud compute instances start "${PENTEST_VM}" \
    --zone="${PENTEST_ZONE}" --project="${PENTEST_PROJECT}" --quiet

# TTL labels — ttl-override-min tells ttl-reaper-dev-vm.sh to use this threshold
# instead of its default 90min. ttl-expire-epoch is a secondary epoch-based marker.
EXPIRE_EPOCH=$(( $(date +%s) + TTL_MINUTES * 60 ))
gcloud compute instances add-labels "${PENTEST_VM}" \
    --zone="${PENTEST_ZONE}" --project="${PENTEST_PROJECT}" \
    --labels="ttl-expire-epoch=${EXPIRE_EPOCH},ttl-override-min=${TTL_MINUTES},pts-build-pipeline=${CI_PIPELINE_NUMBER:-0}"
echo "    TTL label set: expire in ${TTL_MINUTES}min (epoch ${EXPIRE_EPOCH})"

# The woodpecker-agent.service on pentest-dev-vm auto-starts on boot.
# Poll the Woodpecker API until it registers (up to 3 min — accounts for boot time).
echo "==> Waiting for pentest-dev-vm agent to register with Woodpecker..."
for i in $(seq 1 36); do
    FOUND=$(curl -sf --max-time 5 "https://d3ci42.peregrinetechsys.net/api/agents" \
        -H "Authorization: Bearer ${WOODPECKER_API_TOKEN}" 2>/dev/null | \
        python3 -c "
import sys,json,time
agents=json.load(sys.stdin)
now=int(time.time())
found=[a for a in agents if a.get('name','')=='pentest-dev-vm' and now-a.get('last_contact',0)<60]
print(len(found))
" 2>/dev/null || echo 0)
    if [ "${FOUND:-0}" -gt 0 ]; then
        echo "    pentest-dev-vm agent registered (attempt ${i})"
        exit 0
    fi
    echo "    waiting... (attempt ${i}/36, $((i*5))s elapsed)"
    sleep 5
done
echo "ERROR: pentest-dev-vm agent did not register within 180s"
exit 1
