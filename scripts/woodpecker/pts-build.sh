#!/usr/bin/env bash
set -euo pipefail

REGISTRY="us-central1-docker.pkg.dev/ci-runners-de/ci-images"
IMAGE="${REGISTRY}/woodpecker-server"
SHA_SHORT=$(echo "${CI_COMMIT_SHA:-$(git rev-parse HEAD)}" | cut -c1-8)
VERSION="v3.13.0-pts.${CI_PIPELINE_NUMBER:-0}"

echo "==> Building Docker image: ${IMAGE}:${SHA_SHORT}"
echo "    Version: ${VERSION}"

# Authenticate to Artifact Registry using agent SA
gcloud auth configure-docker us-central1-docker.pkg.dev --quiet 2>/dev/null || true

docker build \
  --build-arg "VERSION=${VERSION}" \
  -t "${IMAGE}:${SHA_SHORT}" \
  -t "${IMAGE}:latest" \
  .

echo "==> Pushing to Artifact Registry..."
docker push "${IMAGE}:${SHA_SHORT}"
docker push "${IMAGE}:latest"

echo "==> Build complete: ${IMAGE}:${SHA_SHORT} (${VERSION})"
