#!/usr/bin/env bash
# pts-build.sh — compile woodpecker and publish to GCS for deployment.
# Runs natively on pentest-dev-vm via the pts-build Woodpecker agent (#74).
# Does NOT touch d3ci42 directly — binary is placed in GCS and a pending-deploy
# marker is written. woodpecker-deploy.sh on d3ci42 picks it up via systemd timer.
#
# Secrets: GH_TOKEN (tagging + release)
set -euo pipefail

SHA_SHORT=$(echo "${CI_COMMIT_SHA:-$(git rev-parse HEAD)}" | cut -c1-8)
VERSION="v3.13.0-pts.${CI_PIPELINE_NUMBER:-0}"
COMMIT_SHA="${CI_COMMIT_SHA:-$(git rev-parse HEAD)}"
BUILD_CACHE_BUCKET="gs://ci-runners-de-build-cache"
DEPLOY_BUCKET="${BUILD_CACHE_BUCKET}/woodpecker-deploy"

echo "==> pts-build: ${VERSION} (${SHA_SHORT})"

# ── GCS build cache ──
export PATH="/usr/local/go/bin:$PATH"
GOCACHE="${HOME}/.cache/go-build"
GOMODCACHE="${HOME}/go/pkg/mod"
mkdir -p "${GOCACHE}" "${GOMODCACHE}"

echo "==> Restoring GCS build cache..."
gsutil -m -q rsync -r "${BUILD_CACHE_BUCKET}/go-build/" "${GOCACHE}/" 2>/dev/null && \
    echo "    go-build: $(du -sh "${GOCACHE}" | cut -f1)" || echo "    go-build: cold"
gsutil -m -q rsync -r "${BUILD_CACHE_BUCKET}/go-mod/" "${GOMODCACHE}/" 2>/dev/null && \
    echo "    go-mod: $(du -sh "${GOMODCACHE}" | cut -f1)" || echo "    go-mod: cold"

# ── Web UI ──
echo ""; echo "==> Building web UI..."
cd web && pnpm install --frozen-lockfile >/dev/null 2>&1 && pnpm build >/dev/null 2>&1
echo "    $(ls dist/ | wc -l) files"
cd ..

# ── Compile ──
mkdir -p bin
echo ""; echo "==> Compiling woodpecker-server (CGO=1)..."
CGO_ENABLED=1 go build \
    -ldflags "-s -w -X go.woodpecker-ci.org/woodpecker/v3/version.Version=${VERSION}" \
    -o bin/woodpecker-server ./cmd/server
echo "    $(du -h bin/woodpecker-server | cut -f1)"

echo ""; echo "==> Compiling woodpecker-agent (CGO=0)..."
CGO_ENABLED=0 go build \
    -ldflags "-s -w -X go.woodpecker-ci.org/woodpecker/v3/version.Version=${VERSION}" \
    -o bin/woodpecker-agent ./cmd/agent
echo "    $(du -h bin/woodpecker-agent | cut -f1)"

# Leave agent binary in place for the next pts-wake run — avoids the GitHub
# Release download on every cold-start. The wake step finds this exact version
# and uses it as the pts-build CI agent, ensuring agent/server version parity.
cp bin/woodpecker-agent "/opt/woodpecker/woodpecker-agent-${VERSION}"
chmod +x "/opt/woodpecker/woodpecker-agent-${VERSION}"
echo "    agent binary cached: /opt/woodpecker/woodpecker-agent-${VERSION}"

echo ""; echo "==> Saving GCS build cache..."
gsutil -m -q rsync -r "${GOCACHE}/" "${BUILD_CACHE_BUCKET}/go-build/" 2>/dev/null && \
    echo "    go-build saved" || echo "    ⚠️  go-build save failed (non-fatal)"
gsutil -m -q rsync -r "${GOMODCACHE}/" "${BUILD_CACHE_BUCKET}/go-mod/" 2>/dev/null && \
    echo "    go-mod saved" || echo "    ⚠️  go-mod save failed (non-fatal)"

# ── Upload binary to GCS for deployment ──
# woodpecker-deploy.sh on d3ci42 polls DEPLOY_BUCKET/pending and picks this up.
echo ""; echo "==> Uploading ${VERSION} to GCS for deployment..."
SHA256=$(sha256sum bin/woodpecker-server | awk '{print $1}')
echo "${SHA256}" > bin/woodpecker-server.sha256

gsutil -q cp bin/woodpecker-server "${DEPLOY_BUCKET}/${VERSION}/woodpecker-server"
gsutil -q cp bin/woodpecker-server.sha256 "${DEPLOY_BUCKET}/${VERSION}/woodpecker-server.sha256"

# Write pending-deploy marker last — woodpecker-deploy.sh polls for this
printf '%s\n%s\n%s' "${VERSION}" "${COMMIT_SHA}" "${CI_PIPELINE_NUMBER:-0}" | \
    gsutil -q cp - "${DEPLOY_BUCKET}/pending"
echo "    Binary and pending marker written: ${DEPLOY_BUCKET}/${VERSION}/"
echo "    SHA256: ${SHA256}"

# ── GitHub Release + binary assets ──
REPO_FULL="Peregrine-Technology-Systems/woodpecker"
GH_API="https://api.github.com"
GH_AUTH="Authorization: Bearer ${GH_TOKEN:-}"

if [ -z "${GH_TOKEN:-}" ]; then
    echo "==> Skipping GH Release: GH_TOKEN not set"
    exit 0
fi

echo ""; echo "==> Phase 3a: tag fork at build commit"
if curl -sS -H "${GH_AUTH}" "${GH_API}/repos/${REPO_FULL}/git/refs/tags/${VERSION}" | grep -q '"ref"'; then
    echo "    Tag ${VERSION} already exists; reusing"
else
    TAG_RESP=$(curl -sS -X POST -H "${GH_AUTH}" -H "Content-Type: application/json" \
        "${GH_API}/repos/${REPO_FULL}/git/refs" \
        -d "{\"ref\":\"refs/tags/${VERSION}\",\"sha\":\"${COMMIT_SHA}\"}")
    echo "${TAG_RESP}" | grep -q '"ref"' && echo "    Tag ${VERSION} → ${SHA_SHORT}" || \
        { echo "    ⚠️  Tag failed: $(echo "${TAG_RESP}" | head -c 100)"; exit 0; }
fi

echo ""; echo "==> Phase 3b: create GitHub Release"
RELEASE_BODY="Automated release from pts-build pipeline ${CI_PIPELINE_NUMBER:-?}.\n\nBinaries attached as release assets. Deployed to d3ci42 via GCS pending-deploy pattern (woodpecker#74)."
RELEASE_ID=$(curl -sS -H "${GH_AUTH}" "${GH_API}/repos/${REPO_FULL}/releases/tags/${VERSION}" | \
    python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get("id","") if isinstance(d,dict) else "")' 2>/dev/null || echo "")

if [ -n "${RELEASE_ID}" ]; then
    echo "    Release ${VERSION} already exists (id=${RELEASE_ID})"
else
    RELEASE_RESP=$(curl -sS -X POST -H "${GH_AUTH}" -H "Content-Type: application/json" \
        "${GH_API}/repos/${REPO_FULL}/releases" \
        -d "{\"tag_name\":\"${VERSION}\",\"name\":\"${VERSION}\",\"body\":\"${RELEASE_BODY}\",\"draft\":false,\"prerelease\":false}")
    RELEASE_ID=$(echo "${RELEASE_RESP}" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get("id","") if isinstance(d,dict) else "")' 2>/dev/null || echo "")
    [ -n "${RELEASE_ID}" ] && echo "    Created Release ${VERSION} (id=${RELEASE_ID})" || \
        { echo "    ⚠️  Release failed: $(echo "${RELEASE_RESP}" | head -c 100)"; exit 0; }
fi

upload_asset() {
    local name="$1" path="$2"
    EXISTING=$(curl -sS -H "${GH_AUTH}" "${GH_API}/repos/${REPO_FULL}/releases/${RELEASE_ID}/assets" | \
        python3 -c "import json,sys; d=json.load(sys.stdin); [print(a['id']) for a in (d if isinstance(d,list) else []) if a.get('name')=='${name}']" 2>/dev/null || echo "")
    [ -n "$EXISTING" ] && curl -sS -X DELETE -H "${GH_AUTH}" "${GH_API}/repos/${REPO_FULL}/releases/assets/${EXISTING}" >/dev/null
    RESP=$(curl -sS -X POST -H "${GH_AUTH}" -H "Content-Type: application/octet-stream" \
        --data-binary "@${path}" \
        "https://uploads.github.com/repos/${REPO_FULL}/releases/${RELEASE_ID}/assets?name=${name}")
    echo "$RESP" | grep -q '"browser_download_url"' && \
        echo "    Uploaded ${name} ($(du -h "${path}" | cut -f1))" || \
        echo "    ⚠️  Upload failed for ${name}: $(echo "$RESP" | head -c 100)"
}

upload_asset "woodpecker-server-linux-amd64" "bin/woodpecker-server"
upload_asset "woodpecker-agent-linux-amd64"  "bin/woodpecker-agent"

echo ""; echo "==> Phase 3c: wake ci-image-builder"
BUILDER_PROJECT="ci-runners-de"
BUILDER_ZONE="us-central1-a"
BUILDER_VM="ci-image-builder"
BUILDER_BUCKET="${BUILDER_PROJECT}-image-builder-state"
REQUEST_ID=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n' | awk '{printf "%s-%s-%s-%s-%s", substr($0,1,8), substr($0,9,4), substr($0,13,4), substr($0,17,4), substr($0,21,12)}')
NOW_ISO=$(date -u +%FT%TZ)
JOB_FILE=$(mktemp)
cat > "${JOB_FILE}" << JOB
{"request_id":"${REQUEST_ID}","wp_version":"${VERSION}","scaler_version":"","triggered_by":"woodpecker-pts-build-${CI_PIPELINE_NUMBER:-?}","created_at":"${NOW_ISO}"}
JOB
if gsutil -q cp "${JOB_FILE}" "gs://${BUILDER_BUCKET}/jobs/${REQUEST_ID}.json" 2>/dev/null; then
    echo "    Wrote builder job: ${REQUEST_ID}"
    gcloud --quiet compute instances start "${BUILDER_VM}" --zone="${BUILDER_ZONE}" --project="${BUILDER_PROJECT}" 2>/dev/null && \
        echo "    Started ${BUILDER_VM}" || echo "    ⚠️  Could not start builder VM"
else
    echo "    ⚠️  Could not write builder job — non-blocking"
fi
rm -f "${JOB_FILE}"

echo ""; echo "==> pts-build complete: ${VERSION}"
echo "    Deployment will complete within 2 minutes via woodpecker-deploy.sh on d3ci42."
