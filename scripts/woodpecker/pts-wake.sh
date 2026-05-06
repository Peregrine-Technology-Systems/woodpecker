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

WP_SERVER="d3ci42.peregrinetechsys.net"  # WebSocket via Caddy TLS (port 443)
VERSION="v3.13.0-pts.${CI_PIPELINE_NUMBER:-0}"

# ── Concurrent-run guard ──
# If pentest-dev-vm is already RUNNING and owned by an ACTIVE pipeline, abort.
# Multiple main pushes in quick succession each trigger their own wake; only
# the first one should proceed. A stale label from a failed/killed pipeline is
# NOT sufficient reason to abort — we must verify the owner is still running.
CURRENT_STATUS=$(gcloud compute instances describe "${PENTEST_VM}" \
    --zone="${PENTEST_ZONE}" --project="${PENTEST_PROJECT}" \
    --format="value(status)" 2>/dev/null || echo "UNKNOWN")
if [ "${CURRENT_STATUS}" = "RUNNING" ]; then
    OWNER_PIPELINE=$(gcloud compute instances describe "${PENTEST_VM}" \
        --zone="${PENTEST_ZONE}" --project="${PENTEST_PROJECT}" \
        --format="value(labels.pts-build-pipeline)" 2>/dev/null || echo "")
    if [ -n "${OWNER_PIPELINE}" ] && [ "${OWNER_PIPELINE}" != "0" ]; then
        # Verify the labeled pipeline is still active before deferring to it.
        # A failed/killed pipeline leaves a stale label; don't abort for those.
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
PTS_SSH="ssh -i $SSH_KEY -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout=15 -o ServerAliveInterval=30 -o ServerAliveCountMax=10"

# Brief boot pause — give sshd time to start before the first connection attempt.
# Without this, the kernel accepts port 22 before sshd is ready; SSH hangs
# waiting for a banner rather than getting connection refused, so ConnectTimeout
# is the safety net but the pause makes it a non-issue in normal cases.
sleep 10

# Wait for SSH (up to 2 min)
echo "==> Waiting for SSH..."
for i in $(seq 1 12); do
    $PTS_SSH "root@${PENTEST_IP}" true 2>/dev/null && echo "    ready (attempt ${i})" && break
    [ "$i" -eq 12 ] && echo "ERROR: SSH timeout" && exit 1
    sleep 10
done
# Brief pause after readiness poll — lets UFW's connection-rate window clear
# before the main SSH session (rapid consecutive connections from d3ci42
# trigger limit 22/tcp at 6 conn/30s; binary-check + agent-start in one
# session eliminates the extra connection entirely, but a pause is belt-and-
# suspenders for any connections the readiness loop already issued (#119)).
sleep 3

# Agent secret injected as WOODPECKER_AGENT_SECRET env var via Woodpecker secret
AGENT_SECRET="${WOODPECKER_AGENT_SECRET:?WOODPECKER_AGENT_SECRET must be set}"

# Combine binary check + agent start in ONE SSH session to avoid triggering
# UFW's rate limiter (limit 22/tcp = 6 conn/30s per source IP). Previously
# these were two separate connections immediately following the readiness poll,
# causing the third connection to be rejected mid-wake (#119).
echo "==> Checking agent binary and starting Woodpecker agent on ${PENTEST_VM}..."
AGENT_OUTPUT=$($PTS_SSH "root@${PENTEST_IP}" "
    # ── Binary check ──
    AGENT_BIN=\$(ls /opt/woodpecker/woodpecker-agent-* 2>/dev/null | sort -V | tail -1 || echo '')
    if [ -z \"\$AGENT_BIN\" ]; then
        echo 'NEED_DOWNLOAD'
    else
        echo \"HAVE:\$AGENT_BIN\"
        # ── Agent start (same session) ──
        pkill -f woodpecker-agent 2>/dev/null || true
        sleep 1
        setsid bash -c '
            export WOODPECKER_SERVER=\"${WP_SERVER}\"
            export WOODPECKER_AGENT_SECRET=\"${AGENT_SECRET}\"
            export WOODPECKER_AGENT_TRANSPORT=ws
            export WOODPECKER_GRPC_SECURE=true
            export WOODPECKER_BACKEND=local
            export WOODPECKER_AGENT_LABELS=\"agent=pts-build\"
            export WOODPECKER_HOSTNAME=\"pentest-dev-vm\"
            export WOODPECKER_MAX_WORKFLOWS=1
            export WOODPECKER_GRPC_KEEPALIVE_TIME=10s
            export WOODPECKER_GRPC_KEEPALIVE_TIMEOUT=20s
            exec \"\$AGENT_BIN\" agent
        ' > /tmp/wp-agent.log 2>&1 </dev/null &
        AGENT_PID=\$!
        sleep 2
        if kill -0 \$AGENT_PID 2>/dev/null; then
            echo \"STARTED:\$AGENT_PID\"
            head -3 /tmp/wp-agent.log 2>/dev/null || true
        else
            echo 'DIED'
            cat /tmp/wp-agent.log 2>/dev/null || true
        fi
    fi
")
echo "    ${AGENT_OUTPUT}"

AGENT_BIN=$(echo "${AGENT_OUTPUT}" | grep "^HAVE:" | cut -d: -f2-)
if echo "${AGENT_OUTPUT}" | grep -q "^NEED_DOWNLOAD"; then
    echo "==> No agent binary on ${PENTEST_VM} — downloading from latest GitHub Release..."
    LATEST_TAG=$(curl -sf -H "Authorization: Bearer ${GH_TOKEN:-}" \
        "https://api.github.com/repos/Peregrine-Technology-Systems/woodpecker/releases/latest" | \
        python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["tag_name"])' 2>/dev/null || echo "")
    if [ -z "$LATEST_TAG" ]; then
        echo "ERROR: could not determine latest release tag"
        exit 1
    fi
    AGENT_URL=$(curl -sf -H "Authorization: Bearer ${GH_TOKEN:-}" \
        "https://api.github.com/repos/Peregrine-Technology-Systems/woodpecker/releases/latest" | \
        python3 -c 'import json,sys; d=json.load(sys.stdin); print(next((a["browser_download_url"] for a in d["assets"] if a["name"]=="woodpecker-agent-linux-amd64"), ""))' 2>/dev/null || echo "")
    AGENT_BIN="/opt/woodpecker/woodpecker-agent-${LATEST_TAG}"
    $PTS_SSH "root@${PENTEST_IP}" "
        mkdir -p /opt/woodpecker
        curl -sfL -H 'Authorization: Bearer ${GH_TOKEN:-}' '${AGENT_URL}' \
            -o '${AGENT_BIN}' && chmod +x '${AGENT_BIN}'
        pkill -f woodpecker-agent 2>/dev/null || true
        sleep 1
        setsid bash -c '
            export WOODPECKER_SERVER=\"${WP_SERVER}\"
            export WOODPECKER_AGENT_SECRET=\"${AGENT_SECRET}\"
            export WOODPECKER_AGENT_TRANSPORT=ws
            export WOODPECKER_GRPC_SECURE=true
            export WOODPECKER_BACKEND=local
            export WOODPECKER_AGENT_LABELS=\"agent=pts-build\"
            export WOODPECKER_HOSTNAME=\"pentest-dev-vm\"
            export WOODPECKER_MAX_WORKFLOWS=1
            export WOODPECKER_GRPC_KEEPALIVE_TIME=10s
            export WOODPECKER_GRPC_KEEPALIVE_TIMEOUT=20s
            exec \"\$AGENT_BIN\" agent
        ' > /tmp/wp-agent.log 2>&1 </dev/null &
        sleep 2
        echo 'Downloaded and started: \$AGENT_BIN'
    "
    echo "    downloaded and started: ${AGENT_BIN}"
fi

if echo "${AGENT_OUTPUT}" | grep -q "^DIED"; then
    echo "ERROR: agent exited immediately on ${PENTEST_VM}"
    exit 1
fi

# Poll Woodpecker API until pentest-dev-vm agent registers (up to 150s)
echo "==> Waiting for pentest-dev-vm to register with Woodpecker..."
for i in $(seq 1 30); do
    FOUND=$(curl -sf "https://d3ci42.peregrinetechsys.net/api/agents" \
        -H "Authorization: Bearer ${WOODPECKER_API_TOKEN}" 2>/dev/null | \
        jq -r '[.[].name] | map(select(test("pentest-dev-vm"))) | length' 2>/dev/null || echo 0)
    if [ "${FOUND:-0}" -gt 0 ]; then
        echo "    registered (attempt ${i})"
        exit 0
    fi
    echo "    waiting... (attempt ${i}/30)"
    sleep 5
done
echo "ERROR: pentest-dev-vm agent did not register within 100s"
exit 1
