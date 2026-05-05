#!/usr/bin/env bash
# woodpecker-deploy.sh — standalone woodpecker-server deploy script.
# Runs on d3ci42 as a systemd timer (every 30s). Completely independent of
# Woodpecker pipelines — safe to run while woodpecker-server is being restarted.
#
# Flow:
#   1. Check GCS for pending-deploy marker; exit 0 if absent
#   2. Read version, download binary, verify SHA256
#   3. Stage release: mkdir, cp, chmod, atomic symlink swap
#   4. Restart woodpecker-server via systemctl
#   5. 90s health check — poll /healthz until X-Woodpecker-Version header matches
#   6. On success: write version pin file, prune old releases, remove pending marker, Slack
#   7. On failure: rollback symlink + restart + verify rollback, Slack alert, remove pending marker
#
# Health check: /healthz returns HTTP 204 with empty body. Version is in the
# X-Woodpecker-Version response header. Body parsing will always fail — use header.
#
# Version pin: /opt/woodpecker/server/deployed-version — written atomically on
# every successful deploy. Single source of truth for what is actually running.
# Readable by healthcheck.sh, monitoring, smoke tests.
#
# Alarms: Slack on failure or health-check timeout.
# Idempotent: if pending marker is absent, exits immediately (no-op).
set -euo pipefail

DEPLOY_BUCKET="gs://ci-runners-de-build-cache/woodpecker-deploy"
RELEASES_DIR="/opt/woodpecker/server/releases"
CURRENT_LINK="/opt/woodpecker/server/current"
VERSION_PIN="/opt/woodpecker/server/deployed-version"
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

# healthz_version — query /healthz and return the X-Woodpecker-Version header value.
# healthz is HTTP 204 with empty body; version lives in the response header only.
healthz_version() {
    curl -sf --max-time 5 -D - -o /dev/null "${HEALTH_URL}" 2>/dev/null \
        | grep -i "^X-Woodpecker-Version:" \
        | awk '{print $2}' \
        | tr -d '\r\n' \
        || echo ""
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

echo "$(date -u +%FT%TZ) woodpecker-deploy: deploying ${VERSION} (commit=${COMMIT_SHA:0:8} pipeline=#${PIPELINE_NUM})"

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
# Write to .new then mv atomically — direct cp over the running binary fails
# with "Text file busy" if the same version is already deployed.
mkdir -p "${RELEASES_DIR}/${VERSION}"
cp "${STAGE_DIR}/woodpecker-server" "${RELEASES_DIR}/${VERSION}/woodpecker-server.new"
chmod 755 "${RELEASES_DIR}/${VERSION}/woodpecker-server.new"
mv "${RELEASES_DIR}/${VERSION}/woodpecker-server.new" "${RELEASES_DIR}/${VERSION}/woodpecker-server"
rm -rf "${STAGE_DIR}"

# Atomic symlink swap
ln -sfn "${RELEASES_DIR}/${VERSION}" "${CURRENT_LINK}"
echo "    Symlink: ${CURRENT_LINK} → ${RELEASES_DIR}/${VERSION}"

# ── Restart woodpecker-server ──
echo "    Restarting woodpecker-server..."
systemctl restart woodpecker-server

# ── Health check (90s budget) ──
# /healthz returns HTTP 204 with empty body; version is in X-Woodpecker-Version header.
echo "    Health check (${HEALTH_BUDGET}s budget) — waiting for X-Woodpecker-Version: ${VERSION}..."
HEALTHY=0
DEADLINE=$(( $(date +%s) + HEALTH_BUDGET ))
while [ "$(date +%s)" -lt "${DEADLINE}" ]; do
    VERSION_SERVED=$(healthz_version)
    if [ -n "${VERSION_SERVED}" ]; then
        echo "    healthz: X-Woodpecker-Version=${VERSION_SERVED}"
        if [ "${VERSION_SERVED}" = "${VERSION}" ]; then
            HEALTHY=1
            break
        else
            echo "    version mismatch — served=${VERSION_SERVED} expected=${VERSION}"
        fi
    fi
    sleep 5
done

if [ "${HEALTHY}" -ne 1 ]; then
    echo "ERROR: health check timed out after ${HEALTH_BUDGET}s — rolling back to ${PREVIOUS:-<none>}"
    ROLLBACK_STATUS="no rollback target"
    if [ -n "${PREVIOUS}" ] && [ -d "${RELEASES_DIR}/${PREVIOUS}" ]; then
        ln -sfn "${RELEASES_DIR}/${PREVIOUS}" "${CURRENT_LINK}"
        systemctl restart woodpecker-server
        # Verify rollback — same header check
        sleep 5
        ROLLBACK_VERSION=$(healthz_version)
        if [ "${ROLLBACK_VERSION}" = "${PREVIOUS}" ]; then
            ROLLBACK_STATUS="rolled back to ${PREVIOUS} ✅"
            echo "    ${ROLLBACK_STATUS}"
            slack ":rotating_light:" "deploy ${VERSION} FAILED (health-check timeout). ${ROLLBACK_STATUS}. Pipeline #${PIPELINE_NUM}. Manual investigation required."
        else
            ROLLBACK_STATUS="rollback FAILED (healthz returned '${ROLLBACK_VERSION}')"
            echo "ERROR: ${ROLLBACK_STATUS}"
            slack ":sos:" "deploy ${VERSION} FAILED + ${ROLLBACK_STATUS}. woodpecker-server may be DOWN. Pipeline #${PIPELINE_NUM}. Immediate attention required."
        fi
    else
        slack ":sos:" "deploy ${VERSION} FAILED (health-check timeout) + ${ROLLBACK_STATUS}. woodpecker-server may be DOWN. Pipeline #${PIPELINE_NUM}. Immediate attention required."
    fi
    gsutil -q rm "${DEPLOY_BUCKET}/pending" 2>/dev/null || true
    exit 1
fi

# ── Success — write version pin, prune, clean up ──
echo "✅ ${VERSION} deployed and healthy (X-Woodpecker-Version verified)"

# Write version pin file — well-known location for healthcheck.sh, monitoring, smoke tests
printf '%s\n' "${VERSION}" > "${VERSION_PIN}.new"
mv "${VERSION_PIN}.new" "${VERSION_PIN}"
echo "    Version pin: ${VERSION_PIN} = ${VERSION}"

# Prune old releases (keep KEEP_RELEASES most recent)
find "${RELEASES_DIR}" -maxdepth 1 -mindepth 1 -type d | sort -r | \
    tail -n +$(( KEEP_RELEASES + 1 )) | while read -r dir; do
    current=$(readlink -f "${CURRENT_LINK}" 2>/dev/null || echo "")
    [ "${dir}" != "${current}" ] && rm -rf "${dir}" && echo "    Pruned: ${dir}"
done

# Remove pending marker
gsutil -q rm "${DEPLOY_BUCKET}/pending" 2>/dev/null || true

slack ":white_check_mark:" "woodpecker-server \`${VERSION}\` deployed to d3ci42. Commit: \`${COMMIT_SHA:0:8}\`. Pipeline #${PIPELINE_NUM}."

echo "$(date -u +%FT%TZ) woodpecker-deploy: ${VERSION} complete"
