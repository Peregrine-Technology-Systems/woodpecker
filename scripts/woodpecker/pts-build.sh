#!/usr/bin/env bash
set -euo pipefail

REGISTRY="us-central1-docker.pkg.dev/ci-runners-de/ci-images"
IMAGE="${REGISTRY}/woodpecker-server"
SHA_SHORT=$(echo "${CI_COMMIT_SHA:-$(git rev-parse HEAD)}" | cut -c1-8)
VERSION="v3.13.0-pts.${CI_PIPELINE_NUMBER:-0}"
SERVER_HOST="159.203.159.69"

echo "==> Building Docker image: ${IMAGE}:${VERSION}"

# Authenticate to Artifact Registry using agent SA
gcloud auth configure-docker us-central1-docker.pkg.dev --quiet 2>/dev/null || true

# SSH setup for deploy to d3ci42 (#877)
SSH_KEY=".deploy-ssh/id_ed25519"
mkdir -p .deploy-ssh
echo "$DEPLOY_SSH_KEY" > "$SSH_KEY"
echo "" >> "$SSH_KEY"
chmod 600 "$SSH_KEY"
SSH_OPTS="-i $SSH_KEY -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR"

docker build \
  --build-arg "VERSION=${VERSION}" \
  -t "${IMAGE}:${VERSION}" \
  -t "${IMAGE}:latest" \
  .

echo "==> Pushing to Artifact Registry..."
docker push "${IMAGE}:${VERSION}"
docker push "${IMAGE}:latest"

echo "==> Deploying to d3ci42 (${SERVER_HOST})..."

# Deploy via docker save + SSH (agent has SA key, no AR auth on server)
docker save "${IMAGE}:${VERSION}" | ssh $SSH_OPTS "root@${SERVER_HOST}" "docker load"

# Capture the previous tag for rollback before we rewrite the pin (#33).
PREVIOUS_VERSION=$(ssh $SSH_OPTS "root@${SERVER_HOST}" \
  "grep -oE 'woodpecker-server:v3.13.0-pts\.[0-9]+' /opt/woodpecker/docker-compose.yml | head -1 | sed 's|.*woodpecker-server:||'" || echo "")
echo "==> Previous pin: ${PREVIOUS_VERSION:-<none>}"

# Use flock to prevent concurrent sed race with scaler deploy (#330)
ssh $SSH_OPTS "root@${SERVER_HOST}" "
  flock /opt/woodpecker/docker-compose.yml.lock \
    sed -i 's|woodpecker-server:v3.13.0-pts\.[0-9]*|woodpecker-server:${VERSION}|' /opt/woodpecker/docker-compose.yml
"

echo "==> Image staged: ${VERSION} on d3ci42"

# Recreate the container and verify health (#33). Run under the same flock as
# the sed step so concurrent pts-builds (or pts-build vs scaler deploy) can't
# race on stop/rm/up. On health-check failure, roll the compose pin back to
# the previous tag and bring the old container back up. Same pattern the
# scaler's deploy.sh has used since v0.2.42 (#523, #550).
echo ""
echo "==> Recreating woodpecker-server with ${VERSION} (holding compose lock)"
REMOTE_DEPLOY=$(cat <<REMOTE
set -u
cd /opt/woodpecker

# Phase 1: bring up new image
if ! docker compose up -d --no-deps woodpecker-server; then
  echo "❌ docker compose up failed — rolling back"
  if [ -z "${PREVIOUS_VERSION}" ]; then
    echo "🔥 No previous tag captured — cannot auto-rollback. Server is DOWN."
    exit 1
  fi
  echo "--- Reverting compose pin to ${PREVIOUS_VERSION}"
  flock /opt/woodpecker/docker-compose.yml.lock sed -i "s|woodpecker-server:v3.13.0-pts\.[0-9]*|woodpecker-server:${PREVIOUS_VERSION}|" /opt/woodpecker/docker-compose.yml
  if docker compose up -d --no-deps woodpecker-server; then
    echo "⚠️  Rolled back to ${PREVIOUS_VERSION} — deploy of ${VERSION} FAILED but production is running"
    exit 2
  fi
  echo "🔥 Rollback ALSO failed — server is DOWN. Manual intervention required."
  exit 3
fi

# Phase 2: health-check — 60s budget, 15s intervals.
# /healthz returns 204 when the server is up; /api/queue/info returns 200 + JSON
# when the queue subsystem has finished restoring tasks from the persistent
# store. We verify both before declaring success.
echo "--- Health check: 60s budget, 15s intervals"
HEALTHY=0
for i in 0 1 2 3 4; do
  T=\$((i * 15))
  HEALTHZ=\$(curl -sS -o /dev/null -w "%{http_code}" --max-time 5 https://d3ci42.peregrinetechsys.net/healthz 2>/dev/null || echo "000")
  QUEUE=\$(curl -sS -o /dev/null -w "%{http_code}" --max-time 5 https://d3ci42.peregrinetechsys.net/api/queue/info -H "Authorization: Bearer \${WOODPECKER_API_TOKEN:-}" 2>/dev/null || echo "000")
  echo "  t=\${T}s: /healthz=\$HEALTHZ /api/queue/info=\$QUEUE"
  if [ "\$HEALTHZ" = "204" ] && [ "\$QUEUE" = "200" ]; then
    HEALTHY=1
    break
  fi
  if [ "\$i" -lt 4 ]; then
    sleep 15
  fi
done

if [ "\$HEALTHY" != "1" ]; then
  echo "❌ Health check failed after 60s — rolling back"
  if [ -z "${PREVIOUS_VERSION}" ]; then
    echo "🔥 No previous tag — cannot rollback. Server may be unhealthy."
    exit 1
  fi
  flock /opt/woodpecker/docker-compose.yml.lock sed -i "s|woodpecker-server:v3.13.0-pts\.[0-9]*|woodpecker-server:${PREVIOUS_VERSION}|" /opt/woodpecker/docker-compose.yml
  docker compose up -d --no-deps woodpecker-server
  echo "⚠️  Rolled back to ${PREVIOUS_VERSION} after failed health check"
  exit 2
fi

echo "✅ ${VERSION} is healthy and live"
REMOTE
)

ssh $SSH_OPTS "root@${SERVER_HOST}" "flock /opt/woodpecker/docker-compose.yml.lock bash -s" <<<"$REMOTE_DEPLOY"

# ─────────────────────────────────────────────────────────────────────────────
# Phase 3 (#40): publish agent binary as GitHub Release asset + wake the
# ci-image-builder VM so the matching CI agent VM image gets baked. Each
# block is best-effort and logs without aborting on failure — server image
# has already deployed by this point and is the load-bearing artifact.
# ─────────────────────────────────────────────────────────────────────────────

REPO_FULL="Peregrine-Technology-Systems/woodpecker"
COMMIT_SHA=$(echo "${CI_COMMIT_SHA:-$(git rev-parse HEAD)}")
GH_API="https://api.github.com"
GH_AUTH="Authorization: Bearer ${GH_TOKEN:-}"

if [ -z "${GH_TOKEN:-}" ]; then
  echo ""
  echo "==> Skipping GH Release + builder wake: GH_TOKEN not set"
  echo "    Server image ${IMAGE}:${VERSION} is already deployed; only the"
  echo "    Phase 3 producer-side flow is skipped."
  exit 0
fi

echo ""
echo "==> Phase 3a: extract agent binary from build stage"
AGENT_BIN="/tmp/woodpecker-agent-${VERSION}"
EXTRACT_TAG="pts-agent-extract:${VERSION}"
EXTRACT_CONTAINER="pts-extract-${CI_PIPELINE_NUMBER:-$$}"

if docker build --target build --build-arg "VERSION=${VERSION}" -t "${EXTRACT_TAG}" . >/dev/null 2>&1 \
   && docker create --name "${EXTRACT_CONTAINER}" "${EXTRACT_TAG}" >/dev/null 2>&1 \
   && docker cp "${EXTRACT_CONTAINER}:/build/woodpecker-agent" "${AGENT_BIN}" >/dev/null 2>&1; then
  docker rm "${EXTRACT_CONTAINER}" >/dev/null 2>&1 || true
  echo "    Agent binary: ${AGENT_BIN} ($(du -h "${AGENT_BIN}" | cut -f1))"
else
  docker rm "${EXTRACT_CONTAINER}" >/dev/null 2>&1 || true
  echo "    ⚠️  Agent binary extraction failed; skipping GH Release + builder wake"
  exit 0
fi

echo ""
echo "==> Phase 3b: tag fork at build commit + create GH Release"
# Idempotent tag creation
if curl -sS -H "${GH_AUTH}" "${GH_API}/repos/${REPO_FULL}/git/refs/tags/${VERSION}" | grep -q '"ref"'; then
  echo "    Tag ${VERSION} already exists; reusing"
else
  TAG_RESP=$(curl -sS -X POST -H "${GH_AUTH}" -H "Content-Type: application/json" \
    "${GH_API}/repos/${REPO_FULL}/git/refs" \
    -d "{\"ref\":\"refs/tags/${VERSION}\",\"sha\":\"${COMMIT_SHA}\"}")
  if echo "${TAG_RESP}" | grep -q '"ref"'; then
    echo "    Tag ${VERSION} → ${COMMIT_SHA:0:8}"
  else
    echo "    ⚠️  Failed to create tag ${VERSION}: $(echo "${TAG_RESP}" | head -c 200)"
    exit 0
  fi
fi

# Idempotent Release creation
RELEASE_BODY="Automated release from pts-build pipeline ${CI_PIPELINE_NUMBER:-?}.\n\nServer image: \`${IMAGE}:${VERSION}\` deployed to d3ci42.\nAgent binary: attached as \`woodpecker-agent-linux-amd64\`."
RELEASE_ID=$(curl -sS -H "${GH_AUTH}" "${GH_API}/repos/${REPO_FULL}/releases/tags/${VERSION}" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get("id","") if isinstance(d,dict) else "")' 2>/dev/null || echo "")

if [ -n "${RELEASE_ID}" ]; then
  echo "    Release ${VERSION} already exists (id=${RELEASE_ID}); reusing"
else
  RELEASE_RESP=$(curl -sS -X POST -H "${GH_AUTH}" -H "Content-Type: application/json" \
    "${GH_API}/repos/${REPO_FULL}/releases" \
    -d "{\"tag_name\":\"${VERSION}\",\"name\":\"${VERSION}\",\"body\":\"${RELEASE_BODY}\",\"draft\":false,\"prerelease\":false}")
  RELEASE_ID=$(echo "${RELEASE_RESP}" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get("id","") if isinstance(d,dict) else "")' 2>/dev/null || echo "")
  if [ -n "${RELEASE_ID}" ]; then
    echo "    Created Release ${VERSION} (id=${RELEASE_ID})"
  else
    echo "    ⚠️  Failed to create Release: $(echo "${RELEASE_RESP}" | head -c 200)"
    exit 0
  fi
fi

# Replace asset on Release (idempotent re-runs)
ASSET_NAME="woodpecker-agent-linux-amd64"
EXISTING_ASSET_ID=$(curl -sS -H "${GH_AUTH}" "${GH_API}/repos/${REPO_FULL}/releases/${RELEASE_ID}/assets" \
  | python3 -c "
import json, sys
d = json.load(sys.stdin)
for a in (d if isinstance(d, list) else []):
    if a.get('name') == '${ASSET_NAME}':
        print(a.get('id', ''))
        break
" 2>/dev/null || echo "")

if [ -n "${EXISTING_ASSET_ID}" ]; then
  curl -sS -X DELETE -H "${GH_AUTH}" "${GH_API}/repos/${REPO_FULL}/releases/assets/${EXISTING_ASSET_ID}" >/dev/null
  echo "    Replaced existing ${ASSET_NAME} asset"
fi

UPLOAD_RESP=$(curl -sS -X POST -H "${GH_AUTH}" -H "Content-Type: application/octet-stream" \
  --data-binary "@${AGENT_BIN}" \
  "https://uploads.github.com/repos/${REPO_FULL}/releases/${RELEASE_ID}/assets?name=${ASSET_NAME}")
if echo "${UPLOAD_RESP}" | grep -q '"browser_download_url"'; then
  echo "    Uploaded ${ASSET_NAME} ($(du -h "${AGENT_BIN}" | cut -f1)) to Release ${VERSION}"
else
  echo "    ⚠️  Asset upload failed: $(echo "${UPLOAD_RESP}" | head -c 200)"
fi

echo ""
echo "==> Phase 3c: best-effort wake of ci-image-builder (#1255)"
# Job-file pattern matches ci-infrastructure/terraform/scripts/builder-vm/wake.sh
BUILDER_PROJECT="ci-runners-de"
BUILDER_ZONE="us-central1-a"
BUILDER_VM="ci-image-builder"
BUILDER_BUCKET="${BUILDER_PROJECT}-image-builder-state"
REQUEST_ID=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n' | awk '{
    printf "%s-%s-%s-%s-%s",
        substr($0,1,8), substr($0,9,4), substr($0,13,4),
        substr($0,17,4), substr($0,21,12)
}')
NOW_ISO=$(date -u +%FT%TZ)

JOB_FILE=$(mktemp)
cat > "${JOB_FILE}" <<JOB
{
  "request_id": "${REQUEST_ID}",
  "wp_version": "${VERSION}",
  "scaler_version": "",
  "triggered_by": "woodpecker-fork-pts-build-${CI_PIPELINE_NUMBER:-?}",
  "created_at": "${NOW_ISO}"
}
JOB

if gsutil -q cp "${JOB_FILE}" "gs://${BUILDER_BUCKET}/jobs/${REQUEST_ID}.json" 2>/dev/null; then
  echo "    Wrote job: gs://${BUILDER_BUCKET}/jobs/${REQUEST_ID}.json"
  if gcloud --quiet compute instances start "${BUILDER_VM}" --zone="${BUILDER_ZONE}" --project="${BUILDER_PROJECT}" 2>/dev/null; then
    echo "    Started ${BUILDER_VM} (request_id=${REQUEST_ID})"
  else
    echo "    ⚠️  Could not start builder VM — job written; will be picked up on next wake. Non-blocking."
  fi
else
  echo "    ⚠️  Could not write job file (likely IAM not yet granted to ci-agent SA on ${BUILDER_BUCKET}). Non-blocking — server image already deployed."
fi
rm -f "${JOB_FILE}"

echo ""
echo "✅ Phase 3 producer-side complete: tag ${VERSION}, GH Release with agent binary, wake request ${REQUEST_ID:-skipped}"
