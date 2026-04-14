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

# Use flock to prevent concurrent sed race with scaler deploy (#330)
ssh $SSH_OPTS "root@${SERVER_HOST}" "
  flock /opt/woodpecker/docker-compose.yml.lock \
    sed -i 's|woodpecker-server:v3.13.0-pts\.[0-9]*|woodpecker-server:${VERSION}|' /opt/woodpecker/docker-compose.yml
"

echo "==> Image staged: ${VERSION} on d3ci42"
echo "==> Restart server manually: ssh d3ci42 'cd /opt/woodpecker && docker compose up -d --force-recreate woodpecker-server'"
