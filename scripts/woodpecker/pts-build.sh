#!/usr/bin/env bash
# pts-build.sh — compile woodpecker and stage binaries in GCS for deployment.
# Runs natively on pts-build-vm via the pts-build Woodpecker agent (#140).
#
# (#217) Cache restore/save uses streaming zstd tarballs instead of gsutil
# rsync — eliminates 258 per-object HEAD round-trips, cuts restore from
# ~5-6min to ~1-2min. Persistent data disk dependency removed.
#
# (#219) Does NOT write the pending-deploy marker — that is pts-promote.sh's
# job (runs after pts-build-cleanup). Server restart happens only after the
# full pipeline is done, not mid-cleanup.
#
# Secrets: GH_TOKEN (tagging + release)
set -euo pipefail

SHA_SHORT=$(echo "${CI_COMMIT_SHA:-$(git rev-parse HEAD)}" | cut -c1-8)
VERSION="v3.13.0-pts.${CI_PIPELINE_NUMBER:-0}"
COMMIT_SHA="${CI_COMMIT_SHA:-$(git rev-parse HEAD)}"
BUILD_CACHE_BUCKET="gs://ci-runners-de-build-cache"
DEPLOY_BUCKET="${BUILD_CACHE_BUCKET}/woodpecker-deploy"

echo "==> pts-build: ${VERSION} (${SHA_SHORT})"

# Ensure zstd is available — not always pre-installed on pts-build-vm (#217)
if ! command -v zstd >/dev/null 2>&1; then
    echo "==> Installing zstd..."
    sudo apt-get install -y -q zstd >/dev/null 2>&1 || true
fi

# ── Go toolchain + cache paths ──
export PATH="/usr/local/go/bin:$PATH"
export GOCACHE="${HOME}/.cache/go-build"
export GOMODCACHE="${HOME}/go/pkg/mod"
export GOTELEMETRY=off
mkdir -p "${GOCACHE}" "${GOMODCACHE}"

# (#217) Single-object tarball restore: one GCS GET, no per-object round-trips.
# zstd -d is ~3-4x faster than gzip on Go build artifacts.
echo "==> Restoring go-build cache..."
if gsutil -q cp "${BUILD_CACHE_BUCKET}/go-build-cache.tar.zst" - 2>/dev/null \
    | zstd -d | tar -x -C "${HOME}"; then
    echo "    go-build: $(du -sh "${GOCACHE}" | cut -f1)"
else
    echo "    go-build: cold start"
fi

# go-mod: download only — GCS rsync is slower than network fetch for 3.5GB (#164)
echo "==> Downloading Go modules..."
go mod download 2>/dev/null && echo "    modules ready" || \
    echo "    ⚠️  go mod download had warnings (non-fatal)"

# ── Web UI ──
# (#217) Tarball covers both dist/ and node_modules/ — eliminates the
# pnpm install fallback that fires on every cold start with no cached
# node_modules.
echo ""; echo "==> Restoring web cache (dist + node_modules)..."
mkdir -p web/dist
if gsutil -q cp "${BUILD_CACHE_BUCKET}/web-cache.tar.zst" - 2>/dev/null \
    | zstd -d | tar -x -C web; then
    echo "    dist/: $(ls web/dist/ | wc -l) files (from tarball)"
elif gsutil -m -q rsync -r "${BUILD_CACHE_BUCKET}/woodpecker-web-dist/" web/dist/ 2>/dev/null \
    && [ "$(ls web/dist/ 2>/dev/null | wc -l)" -gt 10 ]; then
    # Migration bridge: tarball not yet primed — fall back to legacy rsync path.
    # woodpecker-web-dist/ is still valid; tarball will be saved at end of this
    # run and future builds will use it. Eliminates pnpm on first migration run.
    echo "    dist/: $(ls web/dist/ | wc -l) files (legacy GCS — priming tarball this run)"
else
    echo "    cache miss — running pnpm build..."
    cd web
    pnpm install --no-frozen-lockfile >/dev/null 2>&1
    node_modules/.bin/vite build --base=/BASE_PATH >/dev/null 2>&1
    cd ..
    echo "    dist/: $(ls web/dist/ | wc -l) files (freshly built)"
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

# ── Save caches ──
echo ""; echo "==> Saving go-build cache..."
if tar -c -C "${HOME}" .cache/go-build \
    | zstd -3 -T0 | gsutil -q cp - "${BUILD_CACHE_BUCKET}/go-build-cache.tar.zst"; then
    echo "    go-build cache saved"
    # One-time migration: remove legacy rsync tree now that tarball is in place (#217).
    # Idempotent — silently skips if already removed.
    if gsutil ls "${BUILD_CACHE_BUCKET}/go-build/" 2>/dev/null | grep -q .; then
        echo "    Removing legacy go-build/ (migrated to tarball)..."
        gsutil -m -q rm -r "${BUILD_CACHE_BUCKET}/go-build/" 2>/dev/null && \
            echo "    Legacy go-build/ removed (12 GiB freed)" || \
            echo "    ⚠️  Legacy go-build/ removal failed (non-fatal)"
    fi
else
    echo "    ⚠️  go-build cache save failed (non-fatal)"
fi

echo "==> Saving web cache (dist + node_modules)..."
tar -c -C web dist node_modules \
    | zstd -3 -T0 | gsutil -q cp - "${BUILD_CACHE_BUCKET}/web-cache.tar.zst" && \
    echo "    web cache saved" || echo "    ⚠️  web cache save failed (non-fatal)"

# ── Upload binaries to GCS ──
echo ""; echo "==> Uploading ${VERSION} to GCS..."
SERVER_SHA256=$(sha256sum bin/woodpecker-server | awk '{print $1}')
echo "${SERVER_SHA256}" > bin/woodpecker-server.sha256
AGENT_SHA256=$(sha256sum bin/woodpecker-agent | awk '{print $1}')
echo "${AGENT_SHA256}" > bin/woodpecker-agent.sha256

gsutil -q cp bin/woodpecker-server "${DEPLOY_BUCKET}/${VERSION}/woodpecker-server"
gsutil -q cp bin/woodpecker-server.sha256 "${DEPLOY_BUCKET}/${VERSION}/woodpecker-server.sha256"
gsutil -q cp bin/woodpecker-agent "${DEPLOY_BUCKET}/${VERSION}/woodpecker-agent"
gsutil -q cp bin/woodpecker-agent.sha256 "${DEPLOY_BUCKET}/${VERSION}/woodpecker-agent.sha256"

echo "    Server + agent uploaded: ${DEPLOY_BUCKET}/${VERSION}/"
echo "    server SHA256: ${SERVER_SHA256}"
echo "    agent  SHA256: ${AGENT_SHA256}"

# Back-compat alias for downstream code that still reads SHA256 (#188)
SHA256="${SERVER_SHA256}"

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

echo ""; echo "==> pts-build compile complete: ${VERSION}"
echo "    Pending marker will be written by pts-promote.sh after cleanup (#219)."
