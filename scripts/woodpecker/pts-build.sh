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
docker save "${IMAGE}:${VERSION}" | ssh -o StrictHostKeyChecking=no "root@${SERVER_HOST}" "docker load"

ssh -o StrictHostKeyChecking=no "root@${SERVER_HOST}" "
  cd /opt/woodpecker
  sed -i 's|woodpecker-server:v3.13.0-pts\.[0-9]*|woodpecker-server:${VERSION}|' docker-compose.yml
  docker compose up -d --force-recreate woodpecker-server
  sleep 5
  docker compose logs woodpecker-server --tail 3
"

echo "==> Deploy complete: ${VERSION} on d3ci42"
