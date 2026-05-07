#!/usr/bin/env bash
# pts-build.sh — compile woodpecker and publish to GCS for deployment.
# Runs natively on pts-build-vm via the pts-build Woodpecker agent (#140).
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
# Use the persistent 200GB data disk for build caches — the 30GB root
# partition fills up when the agent's HOME is a temp workspace directory.
DATA_DISK="/mnt/pts-build-data"
if [ -d "${DATA_DISK}" ] && [ "$(df -P "${DATA_DISK}" | awk 'NR==2{print $4}')" -gt 5000000 ]; then
    GOCACHE="${DATA_DISK}/go-build-cache"
    GOMODCACHE="${DATA_DISK}/go-mod-cache"
    export GOTELEMETRY=off
else
    GOCACHE="${HOME}/.cache/go-build"
    GOMODCACHE="${HOME}/go/pkg/mod"
fi
mkdir -p "${GOCACHE}" "${GOMODCACHE}"

echo "==> Restoring GCS build cache..."
gsutil -m -q rsync -r "${BUILD_CACHE_BUCKET}/go-build/" "${GOCACHE}/" 2>/dev/null && \
    echo "    go-build: $(du -sh "${GOCACHE}" | cut -f1)" || echo "    go-build: cold"
gsutil -m -q rsync -r "${BUILD_CACHE_BUCKET}/go-mod/" "${GOMODCACHE}/" 2>/dev/null && \
    echo "    go-mod: $(du -sh "${GOMODCACHE}" | cut -f1)" || echo "    go-mod: cold"

# ── Web UI ──
# We never change the UI — restore the pre-built dist/ from GCS instead of
# running pnpm on every compile. Only falls back to pnpm if GCS cache is empty.
echo ""; echo "==> Restoring web UI dist/ from GCS cache..."
mkdir -p web/dist
gsutil -m -q rsync -r "${BUILD_CACHE_BUCKET}/woodpecker-web-dist/" web/dist/ 2>/dev/null
DIST_COUNT=$(ls web/dist/ 2>/dev/null | wc -l)
if [ "${DIST_COUNT}" -gt 10 ]; then
    echo "    dist/: ${DIST_COUNT} files (from GCS cache)"
else
    echo "    GCS cache empty or stale — running pnpm build..."
    cd web && pnpm install --no-frozen-lockfile >/dev/null 2>&1 && \
        node_modules/.bin/vite build --base=/BASE_PATH >/dev/null 2>&1
    echo "    dist/: $(ls dist/ | wc -l) files (freshly built)"
    cd ..
    gsutil -m -q rsync -r web/dist/ "${BUILD_CACHE_BUCKET}/woodpecker-web-dist/" 2>/dev/null && \
        echo "    dist/ saved to GCS cache"
fi

# ── Compile ──
mkdir -p bin
echo ""; echo "==> Compiling woodpecker-server (CGO=1)..."
# nice -n 10: lowers build priority so the agent heartbeat goroutine wins CPU
# time during saturation — prevents WS keepalive misses that drop the session (#115).
CGO_ENABLED=1 nice -n 10 go build \
    -ldflags "-s -w -X go.woodpecker-ci.org/woodpecker/v3/version.Version=${VERSION}" \
    -o bin/woodpecker-server ./cmd/server
echo "    $(du -h bin/woodpecker-server | cut -f1)"

echo ""; echo "==> Compiling woodpecker-agent (CGO=0)..."
CGO_ENABLED=0 nice -n 10 go build \
    -ldflags "-s -w -X go.woodpecker-ci.org/woodpecker/v3/version.Version=${VERSION}" \
    -o bin/woodpecker-agent ./cmd/agent
echo "    $(du -h bin/woodpecker-agent | cut -f1)"
# Ephemeral VM (#1669): no local agent binary caching — VM is deleted after compile.
# Agent binary is published via GitHub Release (Phase 3b) for Packer image baking.

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

# ── Create infra tracking issue (SOC 2 CC7.2 traceability) ──
# The ci-image-builder auto-PR (chore/agent-image-bump) must be linked to
# an issue for our standard PR→issue audit trail.
echo ""; echo "==> Phase 3d: create infra tracking issue"
if [ -n "${GH_TOKEN:-}" ]; then
    INFRA_REPO="Peregrine-Technology-Systems/peregrine-infrastructure"
    ISSUE_BODY="## Build record\n\n- **Version:** ${VERSION}\n- **Pipeline:** #${CI_PIPELINE_NUMBER:-?}\n- **Commit:** \`${COMMIT_SHA:0:8}\` on woodpecker fork\n- **Binary SHA256:** \`${SHA256:0:16}...\`\n- **GitHub Release:** https://github.com/Peregrine-Technology-Systems/woodpecker/releases/tag/${VERSION}\n\n## Action\n\nReview and merge the auto-generated \`chore/agent-image-bump\` PR to pin the new ci-agent image to this agent binary. The PR is opened automatically by ci-image-builder.\n\nLink that PR to this issue before merging (SOC 2 CC7.2 traceability)."
    ISSUE_RESP=$(curl -sS -X POST \
        -H "Authorization: Bearer ${GH_TOKEN}" \
        -H "Content-Type: application/json" \
        "https://api.github.com/repos/${INFRA_REPO}/issues" \
        -d "{\"title\":\"chore: pin ci-agent image built from ${VERSION}\",\"body\":\"${ISSUE_BODY}\"}")
    ISSUE_URL=$(echo "${ISSUE_RESP}" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get("html_url",""))' 2>/dev/null || echo "")
    [ -n "${ISSUE_URL}" ] && echo "    Tracking issue: ${ISSUE_URL}" || echo "    ⚠️  Could not create tracking issue (non-blocking)"
else
    echo "    Skipping: GH_TOKEN not set"
fi

echo ""; echo "==> pts-build complete: ${VERSION}"
echo "    Deployment will complete within 2 minutes via woodpecker-deploy.sh on d3ci42."
