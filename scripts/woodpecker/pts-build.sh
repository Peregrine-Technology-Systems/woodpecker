#!/usr/bin/env bash
# pts-build.sh — compile woodpecker and deploy to d3ci42.
# Runs natively on pentest-dev-vm via the pts-build Woodpecker agent (#74).
# Workspace is already checked out by Woodpecker's clone step.
#
# Prerequisites on pentest-dev-vm (Packer image):
#   - /usr/local/go/bin/go + gcc (CGO for go-sqlite3)
#   - node + pnpm (web UI)
#   - gsutil (GCS build cache)
#
# Secrets:
#   DEPLOY_SSH_KEY — SSH key for pentest-dev → d3ci42 deploy
#   GH_TOKEN       — GitHub PAT for tagging + release
set -euo pipefail

SERVER_HOST="159.203.159.69"
RELEASES_DIR="/opt/woodpecker/server/releases"
CURRENT_LINK="/opt/woodpecker/server/current"
KEEP_RELEASES=3

SHA_SHORT=$(echo "${CI_COMMIT_SHA:-$(git rev-parse HEAD)}" | cut -c1-8)
VERSION="v3.13.0-pts.${CI_PIPELINE_NUMBER:-0}"
COMMIT_SHA="${CI_COMMIT_SHA:-$(git rev-parse HEAD)}"

echo "==> pts-build: ${VERSION} (${SHA_SHORT})"

# ── GCS build cache (#851) ──
export PATH="/usr/local/go/bin:$PATH"
BUILD_CACHE_BUCKET="gs://ci-runners-de-build-cache"
GOCACHE="${HOME}/.cache/go-build"
GOMODCACHE="${HOME}/go/pkg/mod"
mkdir -p "${GOCACHE}" "${GOMODCACHE}"

echo "==> Restoring GCS build cache..."
gsutil -m -q rsync -r "${BUILD_CACHE_BUCKET}/go-build/" "${GOCACHE}/" 2>/dev/null && \
    echo "    go-build: $(du -sh "${GOCACHE}" | cut -f1)" || echo "    go-build: cold"
gsutil -m -q rsync -r "${BUILD_CACHE_BUCKET}/go-mod/" "${GOMODCACHE}/" 2>/dev/null && \
    echo "    go-mod: $(du -sh "${GOMODCACHE}" | cut -f1)" || echo "    go-mod: cold"

# ── Build web UI ──
echo ""
echo "==> Building web UI..."
cd web && pnpm install --frozen-lockfile >/dev/null 2>&1 && pnpm build >/dev/null 2>&1
echo "    $(ls dist/ | wc -l) files"
cd ..

# ── Compile binaries ──
mkdir -p bin

echo ""
echo "==> Compiling woodpecker-server (CGO=1)..."
CGO_ENABLED=1 go build \
    -ldflags "-s -w -X go.woodpecker-ci.org/woodpecker/v3/version.Version=${VERSION}" \
    -o bin/woodpecker-server ./cmd/server
echo "    $(du -h bin/woodpecker-server | cut -f1)"

echo ""
echo "==> Compiling woodpecker-agent (CGO=0)..."
CGO_ENABLED=0 go build \
    -ldflags "-s -w -X go.woodpecker-ci.org/woodpecker/v3/version.Version=${VERSION}" \
    -o bin/woodpecker-agent ./cmd/agent
echo "    $(du -h bin/woodpecker-agent | cut -f1)"

echo ""
echo "==> Saving GCS build cache..."
gsutil -m -q rsync -r "${GOCACHE}/" "${BUILD_CACHE_BUCKET}/go-build/" 2>/dev/null && \
    echo "    go-build saved" || echo "    ⚠️  go-build save failed (non-fatal)"
gsutil -m -q rsync -r "${GOMODCACHE}/" "${BUILD_CACHE_BUCKET}/go-mod/" 2>/dev/null && \
    echo "    go-mod saved" || echo "    ⚠️  go-mod save failed (non-fatal)"

# ── SSH setup for d3ci42 deploy ──
SSH_KEY=".deploy-ssh/id_ed25519"
mkdir -p .deploy-ssh
echo "$DEPLOY_SSH_KEY" > "$SSH_KEY"
printf '\n' >> "$SSH_KEY"
chmod 600 "$SSH_KEY"
SSH="ssh -i $SSH_KEY -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR"
ssh_cmd() { $SSH "root@${SERVER_HOST}" "$@"; }

# ── Deploy server to d3ci42 ──
echo ""
echo "==> Deploying ${VERSION} to ${SERVER_HOST}..."
PREVIOUS=$(ssh_cmd "readlink -f ${CURRENT_LINK} 2>/dev/null | xargs basename 2>/dev/null || echo ''" || echo "")
echo "    Previous: ${PREVIOUS:-<none>}"

ssh_cmd "mkdir -p ${RELEASES_DIR}/${VERSION}"
rsync -az --checksum \
    -e "ssh -i $SSH_KEY -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR" \
    bin/woodpecker-server "root@${SERVER_HOST}:${RELEASES_DIR}/${VERSION}/woodpecker-server"
ssh_cmd "chmod 755 ${RELEASES_DIR}/${VERSION}/woodpecker-server"

LOCAL_SHA=$(sha256sum bin/woodpecker-server | awk '{print $1}')
REMOTE_SHA=$(ssh_cmd "sha256sum ${RELEASES_DIR}/${VERSION}/woodpecker-server | awk '{print \$1}'")
if [ "$LOCAL_SHA" != "$REMOTE_SHA" ]; then
    echo "ERROR: checksum mismatch after rsync"
    exit 1
fi
echo "    checksum verified: ${LOCAL_SHA:0:16}..."

ssh_cmd "
    ln -sfn ${RELEASES_DIR}/${VERSION} ${CURRENT_LINK}
    systemctl daemon-reload 2>/dev/null || true
    systemctl restart woodpecker-server
"

# ── Health check (60s budget) ──
echo ""
echo "==> Health check..."
HEALTHY=0
for i in 0 1 2 3 4; do
    T=$((i * 15))
    RESPONSE=$(ssh_cmd "curl -sf --max-time 5 http://localhost:8000/healthz 2>/dev/null || echo ''" || echo "")
    if [ -z "$RESPONSE" ]; then
        echo "  t=${T}s: <unreachable>"
    else
        VERSION_SERVED=$(echo "$RESPONSE" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get("version","?"))' 2>/dev/null || echo "?")
        echo "  t=${T}s: version=${VERSION_SERVED}"
        if echo "$RESPONSE" | grep -q '"status":"ok"'; then
            HEALTHY=1
            break
        fi
    fi
    [ "$i" -lt 4 ] && sleep 15
done

if [ "$HEALTHY" -ne 1 ]; then
    echo "❌ Health check failed — rolling back to ${PREVIOUS:-<none>}"
    if [ -n "$PREVIOUS" ] && ssh_cmd "test -d ${RELEASES_DIR}/${PREVIOUS}"; then
        ssh_cmd "
            ln -sfn ${RELEASES_DIR}/${PREVIOUS} ${CURRENT_LINK}
            systemctl restart woodpecker-server
        "
        echo "⚠️  Rolled back to ${PREVIOUS}"
    fi
    exit 1
fi

ssh_cmd "
    find ${RELEASES_DIR} -maxdepth 1 -mindepth 1 -type d | sort -r | tail -n +$((KEEP_RELEASES + 1)) | while read -r dir; do
        current=\$(readlink -f ${CURRENT_LINK} 2>/dev/null || echo '')
        [ \"\$dir\" != \"\$current\" ] && rm -rf \"\$dir\" && echo \"Pruned: \$dir\"
    done
" 2>/dev/null || true

echo "✅ ${VERSION} deployed to ${SERVER_HOST}"

# ── Phase 3: GitHub Release + binary assets + builder VM wake ──
REPO_FULL="Peregrine-Technology-Systems/woodpecker"
GH_API="https://api.github.com"
GH_AUTH="Authorization: Bearer ${GH_TOKEN:-}"

if [ -z "${GH_TOKEN:-}" ]; then
    echo "==> Skipping GH Release: GH_TOKEN not set"
    exit 0
fi

echo ""
echo "==> Phase 3a: tag fork at build commit"
if curl -sS -H "${GH_AUTH}" "${GH_API}/repos/${REPO_FULL}/git/refs/tags/${VERSION}" | grep -q '"ref"'; then
    echo "    Tag ${VERSION} already exists; reusing"
else
    TAG_RESP=$(curl -sS -X POST -H "${GH_AUTH}" -H "Content-Type: application/json" \
        "${GH_API}/repos/${REPO_FULL}/git/refs" \
        -d "{\"ref\":\"refs/tags/${VERSION}\",\"sha\":\"${COMMIT_SHA}\"}")
    echo "${TAG_RESP}" | grep -q '"ref"' && echo "    Tag ${VERSION} → ${SHA_SHORT}" || \
        { echo "    ⚠️  Tag failed: $(echo "${TAG_RESP}" | head -c 100)"; exit 0; }
fi

echo ""
echo "==> Phase 3b: create GitHub Release"
RELEASE_BODY="Automated release from pts-build pipeline ${CI_PIPELINE_NUMBER:-?}.\n\nBinaries attached as release assets. Deployed to d3ci42 via native rsync (#57, #74)."
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

echo ""
echo "==> Phase 3c: wake ci-image-builder"
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
    echo "    Wrote job: ${REQUEST_ID}"
    gcloud --quiet compute instances start "${BUILDER_VM}" --zone="${BUILDER_ZONE}" --project="${BUILDER_PROJECT}" 2>/dev/null && \
        echo "    Started ${BUILDER_VM}" || echo "    ⚠️  Could not start builder VM — job queued"
else
    echo "    ⚠️  Could not write job file — non-blocking"
fi
rm -f "${JOB_FILE}"

echo ""
echo "==> pts-build complete: ${VERSION}"
