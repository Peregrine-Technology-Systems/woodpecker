#!/usr/bin/env bash
# pts-wake.sh — start pentest-dev-vm and register it as a Woodpecker agent.
# Runs on d3ci42-local (backend: local). Lightweight — no compilation here.
#
# Sets a TTL label on the VM as a safety net: if the cleanup workflow fails
# to stop the VM, the TTL reaper on d3ci42 will catch it.
set -euo pipefail

PENTEST_PROJECT="peregrine-pentest-dev"
PENTEST_ZONE="us-central1-a"
PENTEST_VM="pentest-dev-vm"
TTL_MINUTES=60  # generous budget: wake + compile + deploy

WP_SERVER="d3ci42.peregrinetechsys.net:443"  # Caddy proxies gRPC → port 9000
VERSION="v3.13.0-pts.${CI_PIPELINE_NUMBER:-0}"

echo "==> Starting ${PENTEST_VM} for pts-build ${VERSION}..."
gcloud compute instances start "${PENTEST_VM}" \
    --zone="${PENTEST_ZONE}" --project="${PENTEST_PROJECT}" --quiet

# TTL label — safety net if cleanup workflow doesn't run (#74)
EXPIRE_EPOCH=$(( $(date +%s) + TTL_MINUTES * 60 ))
gcloud compute instances add-labels "${PENTEST_VM}" \
    --zone="${PENTEST_ZONE}" --project="${PENTEST_PROJECT}" \
    --labels="ttl-expire-epoch=${EXPIRE_EPOCH},pts-build-pipeline=${CI_PIPELINE_NUMBER:-0}"
echo "    TTL label set: expire in ${TTL_MINUTES}min (epoch ${EXPIRE_EPOCH})"

# Get public IP
PENTEST_IP=$(gcloud compute instances describe "${PENTEST_VM}" \
    --zone="${PENTEST_ZONE}" --project="${PENTEST_PROJECT}" \
    --format="value(networkInterfaces[0].accessConfigs[0].natIP)")
echo "    IP: ${PENTEST_IP}"

# SSH key setup
SSH_KEY=".deploy-ssh/pts-build-key"
mkdir -p .deploy-ssh
echo "$PTS_BUILD_SSH_KEY" > "$SSH_KEY"
printf '\n' >> "$SSH_KEY"
chmod 600 "$SSH_KEY"
PTS_SSH="ssh -i $SSH_KEY -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ServerAliveInterval=30 -o ServerAliveCountMax=10"

# Wait for SSH (up to 2 min)
echo "==> Waiting for SSH..."
for i in $(seq 1 12); do
    $PTS_SSH "root@${PENTEST_IP}" true 2>/dev/null && echo "    ready (attempt ${i})" && break
    [ "$i" -eq 12 ] && echo "ERROR: SSH timeout" && exit 1
    sleep 10
done

# Agent secret injected as WOODPECKER_AGENT_SECRET env var via Woodpecker secret
AGENT_SECRET="${WOODPECKER_AGENT_SECRET:?WOODPECKER_AGENT_SECRET must be set}"

# Find the woodpecker-agent binary on pentest-dev
AGENT_BIN=$($PTS_SSH "root@${PENTEST_IP}" \
    "ls /opt/woodpecker/woodpecker-agent-* 2>/dev/null | sort -V | tail -1 || echo ''")
if [ -z "${AGENT_BIN}" ]; then
    echo "ERROR: no woodpecker-agent binary found on ${PENTEST_VM}"
    exit 1
fi
echo "    agent binary: ${AGENT_BIN}"

# Start woodpecker-agent on pentest-dev with pts-build label
echo "==> Starting Woodpecker agent on ${PENTEST_VM}..."
$PTS_SSH "root@${PENTEST_IP}" "
    pkill -f woodpecker-agent 2>/dev/null || true
    nohup env \
        WOODPECKER_SERVER='${WP_SERVER}' \
        WOODPECKER_AGENT_SECRET='${AGENT_SECRET}' \
        WOODPECKER_BACKEND=local \
        WOODPECKER_AGENT_LABELS='agent=pts-build' \
        WOODPECKER_HOSTNAME='pentest-dev-vm' \
        WOODPECKER_MAX_WORKFLOWS=1 \
        '${AGENT_BIN}' agent \
        > /tmp/wp-agent.log 2>&1 &
    echo \"Agent started (PID \$!)\"
"

# Poll Woodpecker API until pentest-dev-vm agent registers (up to 100s)
echo "==> Waiting for pentest-dev-vm to register with Woodpecker..."
for i in $(seq 1 20); do
    FOUND=$(curl -sf "https://d3ci42.peregrinetechsys.net/api/agents" \
        -H "Authorization: Bearer ${WOODPECKER_API_TOKEN}" 2>/dev/null | \
        jq -r '[.[].name] | map(select(. == "pentest-dev-vm")) | length' 2>/dev/null || echo 0)
    if [ "${FOUND:-0}" -gt 0 ]; then
        echo "    registered (attempt ${i})"
        exit 0
    fi
    sleep 5
done
echo "ERROR: pentest-dev-vm agent did not register within 100s"
exit 1
