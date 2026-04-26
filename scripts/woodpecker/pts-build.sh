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
