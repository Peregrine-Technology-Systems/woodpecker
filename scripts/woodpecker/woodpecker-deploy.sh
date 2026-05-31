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

# ── Helpers (sourceable lib — hardened against the #3089 silent-OK class) ──
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/woodpecker/lib/deploy-helpers.sh
source "${SCRIPT_DIR}/lib/deploy-helpers.sh"

# API token secret (#92). The real Woodpecker API token lives in `ci-api-token`;
# the old `woodpecker-api-token` name does not exist in GCP SM (NOT_FOUND), so
# rotation silently never ran. See lib/deploy-helpers.sh::rotate_api_token.
API_TOKEN_SECRET="ci-api-token"
GCP_PROJECT="ci-runners-de"
SERVER_URL="http://localhost:8000"

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
# read_pending_marker distinguishes "no marker" (idle, quiet) from a backend
# error (could not determine — must NOT be silently treated as idle, #3089).
PENDING_CONTENT="$(read_pending_marker "${DEPLOY_BUCKET}")"
case $? in
    0) : ;;          # marker present — proceed
    1) exit 0 ;;     # genuinely no pending deploy — common idle case
    *)               # backend error — surface loudly, do not masquerade as idle
        echo "ERROR: could not read pending marker from ${DEPLOY_BUCKET} — treating as a failure, not as idle"
        slack ":warning:" "deploy timer could not read the pending marker (GCS error) — check gsutil auth/connectivity on d3ci42."
        exit 1
        ;;
esac

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

# ── Enforce Wants= (not Requires=) in woodpecker-agent.service ──
# Requires= cascades server restarts to the agent, killing in-flight pipelines.
# Wants= starts the agent after the server but never stops it on server restart (#112).
AGENT_UNIT="/etc/systemd/system/woodpecker-agent.service"
if [ -f "${AGENT_UNIT}" ] && grep -q "^Requires=woodpecker-server" "${AGENT_UNIT}" 2>/dev/null; then
    echo "    Fixing woodpecker-agent.service: Requires= → Wants="
    sed -i 's/^Requires=woodpecker-server/Wants=woodpecker-server/' "${AGENT_UNIT}"
    systemctl daemon-reload
    echo "    woodpecker-agent.service fixed (daemon-reloaded)"
fi

# ── Enforce agent.env labels ──
# The d3ci42-local agent must advertise ONLY backend=local — never platform=linux.
# platform=linux causes it to compete with GCP VMs for regular CI tasks, running
# them on the same 2GB droplet as the server (OOM risk, contention). Idempotent.
AGENT_ENV="/etc/woodpecker/agent.env"
if [ -f "${AGENT_ENV}" ]; then
    if grep -q "platform=linux" "${AGENT_ENV}" 2>/dev/null; then
        echo "    Fixing agent.env: removing platform=linux from WOODPECKER_AGENT_LABELS"
        grep -v "^WOODPECKER_AGENT_LABELS=" "${AGENT_ENV}" > "${AGENT_ENV}.new"
        echo "WOODPECKER_AGENT_LABELS=backend=local" >> "${AGENT_ENV}.new"
        mv "${AGENT_ENV}.new" "${AGENT_ENV}"
        systemctl restart woodpecker-agent || true
        echo "    agent.env fixed + agent restarted"
    fi
fi

# ── Auto-rotate JWT secret and API token (#92) ──
echo "$(date -u +%FT%TZ) woodpecker-deploy: rotating JWT secret and API token..."

NEW_JWT_SECRET=$(openssl rand -hex 32)

# Update EnvironmentFile so woodpecker-server picks up the new secret on next restart
if grep -q "^WOODPECKER_JWT_SECRET=" /etc/woodpecker/secrets.env 2>/dev/null; then
    sed -i "s|^WOODPECKER_JWT_SECRET=.*|WOODPECKER_JWT_SECRET=${NEW_JWT_SECRET}|" /etc/woodpecker/secrets.env
else
    echo "WOODPECKER_JWT_SECRET=${NEW_JWT_SECRET}" >> /etc/woodpecker/secrets.env
fi

# Store new JWT secret in GCP SM
gcloud secrets versions add woodpecker--jwt-secret \
    --data-file=<(printf '%s' "${NEW_JWT_SECRET}") \
    --project=ci-runners-de 2>/dev/null || \
gcloud secrets create woodpecker--jwt-secret \
    --data-file=<(printf '%s' "${NEW_JWT_SECRET}") \
    --project=ci-runners-de

# Restart server to pick up new JWT secret; wait for healthy
systemctl restart woodpecker-server
JWT_HEALTH_DEADLINE=$(( $(date +%s) + HEALTH_BUDGET ))
while true; do
    if [ "$(date +%s)" -gt "${JWT_HEALTH_DEADLINE}" ]; then
        slack ":warning:" "JWT rotation: server did not come healthy after secret rotation — manual check required (#92)"
        break
    fi
    RUNNING_VERSION=$(healthz_version)
    if [ "${RUNNING_VERSION}" = "${VERSION}" ]; then
        break
    fi
    sleep 3
done

# Refresh the API token by calling the server's own endpoint (user ID 1).
# The API token is signed with user.Hash (stored in DB), not WOODPECKER_JWT_SECRET,
# so we ask the server to sign a fresh one rather than constructing it here.
# rotate_api_token branches on exit status (error vs genuinely-absent, no
# silent-OK swallow) and verifies the new token works before overwriting the
# stored one (Act → Verify). See lib/deploy-helpers.sh.
ROTATE_OUT="$(rotate_api_token "${API_TOKEN_SECRET}" "${GCP_PROJECT}" "${SERVER_URL}")"
ROTATE_RC=$?
echo "$(date -u +%FT%TZ) woodpecker-deploy: ${ROTATE_OUT}"
case "${ROTATE_RC}" in
    0) : ;;  # rotated + verified
    1) : ;;  # secret genuinely absent — informational, nothing to refresh
    *) slack ":warning:" "JWT rotation: ${ROTATE_OUT}" ;;  # real error — alert
esac

# Remove pending marker
gsutil -q rm "${DEPLOY_BUCKET}/pending" 2>/dev/null || true

slack ":white_check_mark:" "woodpecker-server \`${VERSION}\` deployed to d3ci42. Commit: \`${COMMIT_SHA:0:8}\`. Pipeline #${PIPELINE_NUM}."

echo "$(date -u +%FT%TZ) woodpecker-deploy: ${VERSION} complete"
