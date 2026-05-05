#!/usr/bin/env bash
# woodpecker-deploy.sh — standalone woodpecker-server deploy script.
# Runs on d3ci42 as a systemd timer (every 30s). Completely independent of
# Woodpecker pipelines — safe to run while woodpecker-server is being restarted.
#
# Flow:
#   1. Check GCS for pending-deploy marker; exit 0 if absent
#   2. Read version, download binary, verify SHA256
#   3. Stage release: mkdir, rsync, chmod, symlink (atomic)
#   4. Restart woodpecker-server via systemctl
#   5. 90s health check — poll /healthz until version matches or timeout
#   6. On failure: rollback symlink + restart, Slack alert, remove pending marker
#   7. On success: prune old releases, remove pending marker, Slack success
#
# Alarms: Slack message on failure or health-check timeout.
# Idempotent: if pending marker is absent, exits immediately (no-op).
set -euo pipefail

DEPLOY_BUCKET="gs://ci-runners-de-build-cache/woodpecker-deploy"
RELEASES_DIR="/opt/woodpecker/server/releases"
CURRENT_LINK="/opt/woodpecker/server/current"
KEEP_RELEASES=3
HEALTH_URL="http://localhost:8000/healthz"
HEALTH_BUDGET=90  # seconds
LOCK_FILE="/tmp/woodpecker-deploy.lock"

# ── Secrets ──
source /etc/woodpecker/secrets.env 2>/dev/null || true
SLACK_URL="${SLACK_WEBHOOK_URL:-}"

slack() {
    local emoji="$1" msg="$2"
    [ -z "${SLACK_URL}" ] && return 0
    curl -sf -X POST "${SLACK_URL}" -H "Content-Type: application/json" \
        -d "{\"text\":\"${emoji} *woodpecker-deploy* ${msg}\"}" >/dev/null 2>&1 || true
}

# ── Exclusive lock — prevent concurrent runs ──
exec 9>"${LOCK_FILE}"
if ! flock -n 9; then
    echo "Another woodpecker-deploy.sh is running — exiting"
    exit 0
fi

# ── Check for pending deploy ──
PENDING_CONTENT=$(gsutil -q cat "${DEPLOY_BUCKET}/pending" 2>/dev/null || echo "")
if [ -z "${PENDING_CONTENT}" ]; then
    exit 0  # nothing to deploy
fi

VERSION=$(echo "${PENDING_CONTENT}" | head -1)
COMMIT_SHA=$(echo "${PENDING_CONTENT}" | sed -n '2p')
PIPELINE_NUM=$(echo "${PENDING_CONTENT}" | sed -n '3p')

echo "$(date -u +%FT%TZ) woodpecker-deploy: deploying ${VERSION} (pipeline #${PIPELINE_NUM})"

# ── Capture previous release for rollback ──
PREVIOUS=$(readlink -f "${CURRENT_LINK}" 2>/dev/null | xargs basename 2>/dev/null || echo "")
echo "    Previous: ${PREVIOUS:-<none>}"

# ── Download and verify binary ──
STAGE_DIR="/tmp/woodpecker-stage-${VERSION}"
rm -rf "${STAGE_DIR}"
mkdir -p "${STAGE_DIR}"

echo "    Downloading ${VERSION} from GCS..."
if ! gsutil -q cp "${DEPLOY_BUCKET}/${VERSION}/woodpecker-server" "${STAGE_DIR}/woodpecker-server" 2>/dev/null; then
    echo "ERROR: binary not found in GCS: ${DEPLOY_BUCKET}/${VERSION}/woodpecker-server"
    slack ":x:" "deploy ${VERSION} FAILED — binary not found in GCS. Pipeline #${PIPELINE_NUM}."
    gsutil -q rm "${DEPLOY_BUCKET}/pending" 2>/dev/null || true
    rm -rf "${STAGE_DIR}"
    exit 1
fi

EXPECTED_SHA=$(gsutil -q cat "${DEPLOY_BUCKET}/${VERSION}/woodpecker-server.sha256" 2>/dev/null || echo "")
ACTUAL_SHA=$(sha256sum "${STAGE_DIR}/woodpecker-server" | awk '{print $1}')
if [ -z "${EXPECTED_SHA}" ] || [ "${EXPECTED_SHA}" != "${ACTUAL_SHA}" ]; then
    echo "ERROR: SHA256 mismatch — expected=${EXPECTED_SHA:-missing} actual=${ACTUAL_SHA}"
    slack ":x:" "deploy ${VERSION} FAILED — SHA256 mismatch. Expected: \`${EXPECTED_SHA:0:12}...\` Got: \`${ACTUAL_SHA:0:12}...\`. Pipeline #${PIPELINE_NUM}."
    gsutil -q rm "${DEPLOY_BUCKET}/pending" 2>/dev/null || true
    rm -rf "${STAGE_DIR}"
    exit 1
fi
echo "    SHA256 verified: ${ACTUAL_SHA:0:16}..."

# ── Stage release ──
mkdir -p "${RELEASES_DIR}/${VERSION}"
cp "${STAGE_DIR}/woodpecker-server" "${RELEASES_DIR}/${VERSION}/woodpecker-server"
chmod 755 "${RELEASES_DIR}/${VERSION}/woodpecker-server"
rm -rf "${STAGE_DIR}"

# Atomic symlink swap
ln -sfn "${RELEASES_DIR}/${VERSION}" "${CURRENT_LINK}"
echo "    Symlink: ${CURRENT_LINK} → ${RELEASES_DIR}/${VERSION}"

# ── Restart woodpecker-server ──
echo "    Restarting woodpecker-server..."
systemctl restart woodpecker-server

# ── Health check (90s budget) ──
echo "    Health check (${HEALTH_BUDGET}s budget)..."
HEALTHY=0
DEADLINE=$(( $(date +%s) + HEALTH_BUDGET ))
while [ "$(date +%s)" -lt "${DEADLINE}" ]; do
    RESPONSE=$(curl -sf --max-time 3 "${HEALTH_URL}" 2>/dev/null || echo "")
    if [ -n "${RESPONSE}" ]; then
        STATUS=$(echo "${RESPONSE}" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get("status",""))' 2>/dev/null || echo "")
        VERSION_SERVED=$(echo "${RESPONSE}" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get("version",""))' 2>/dev/null || echo "")
        echo "    healthz: status=${STATUS} version=${VERSION_SERVED}"
        if [ "${STATUS}" = "ok" ] && [ "${VERSION_SERVED}" = "${VERSION}" ]; then
            HEALTHY=1
            break
        fi
    fi
    sleep 5
done

if [ "${HEALTHY}" -ne 1 ]; then
    echo "ERROR: health check timed out after ${HEALTH_BUDGET}s — rolling back to ${PREVIOUS:-<none>}"
    # Rollback
    if [ -n "${PREVIOUS}" ] && [ -d "${RELEASES_DIR}/${PREVIOUS}" ]; then
        ln -sfn "${RELEASES_DIR}/${PREVIOUS}" "${CURRENT_LINK}"
        systemctl restart woodpecker-server
        sleep 5
        ROLLBACK_OK=$(curl -sf --max-time 5 "${HEALTH_URL}" 2>/dev/null | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get("status",""))' 2>/dev/null || echo "")
        if [ "${ROLLBACK_OK}" = "ok" ]; then
            echo "    Rolled back to ${PREVIOUS} — server healthy"
            slack ":rotating_light:" "deploy ${VERSION} FAILED (health-check timeout). Rolled back to \`${PREVIOUS}\`. Server healthy. Pipeline #${PIPELINE_NUM}. Manual investigation required."
        else
            slack ":sos:" "deploy ${VERSION} FAILED + rollback FAILED. woodpecker-server may be DOWN. Pipeline #${PIPELINE_NUM}. Immediate attention required."
        fi
    else
        slack ":sos:" "deploy ${VERSION} FAILED (health-check timeout) + no rollback target. woodpecker-server may be DOWN. Pipeline #${PIPELINE_NUM}. Immediate attention required."
    fi
    gsutil -q rm "${DEPLOY_BUCKET}/pending" 2>/dev/null || true
    exit 1
fi

# ── Success ──
echo "✅ ${VERSION} deployed and healthy"

# Prune old releases (keep KEEP_RELEASES most recent)
find "${RELEASES_DIR}" -maxdepth 1 -mindepth 1 -type d | sort -r | \
    tail -n +$(( KEEP_RELEASES + 1 )) | while read -r dir; do
    current=$(readlink -f "${CURRENT_LINK}" 2>/dev/null || echo "")
    [ "${dir}" != "${current}" ] && rm -rf "${dir}" && echo "    Pruned: ${dir}"
done

# Remove pending marker
gsutil -q rm "${DEPLOY_BUCKET}/pending" 2>/dev/null || true

slack ":white_check_mark:" "woodpecker-server \`${VERSION}\` deployed to d3ci42. Pipeline #${PIPELINE_NUM}. Commit: \`${COMMIT_SHA:0:8}\`."

echo "$(date -u +%FT%TZ) woodpecker-deploy: ${VERSION} complete"
