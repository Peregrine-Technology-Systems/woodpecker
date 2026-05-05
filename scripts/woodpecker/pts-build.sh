#!/usr/bin/env bash
# pts-build.sh — compile woodpecker on pentest-dev-vm, deploy binaries to d3ci42.
#
# Flow (#67):
#   1. Start pentest-dev-vm (GCP)
#   2. SSH → pentest-dev: git clone, restore GCS cache, pnpm + go build
#   3. rsync bin/ back to CI agent working dir
#   4. Stop pentest-dev-vm (trap — runs on success or failure)
#   5. rsync server binary → d3ci42 + symlink + systemctl restart
#   6. Health check (60s budget, rollback on failure)
#   7. GitHub Release + binary assets
#   8. ci-image-builder wake
#
# Secrets:
#   DEPLOY_SSH_KEY     — SSH key for CI agent → d3ci42
#   PTS_BUILD_SSH_KEY  — SSH key for CI agent → pentest-dev-vm (#67)
#   GH_TOKEN           — GitHub PAT for tagging + release
set -euo pipefail

SERVER_HOST="159.203.159.69"
RELEASES_DIR="/opt/woodpecker/server/releases"
CURRENT_LINK="/opt/woodpecker/server/current"
KEEP_RELEASES=3

PENTEST_PROJECT="peregrine-pentest-dev"
PENTEST_ZONE="us-central1-a"
PENTEST_VM="pentest-dev-vm"

SHA_SHORT=$(echo "${CI_COMMIT_SHA:-$(git rev-parse HEAD)}" | cut -c1-8)
VERSION="v3.13.0-pts.${CI_PIPELINE_NUMBER:-0}"
COMMIT_SHA="${CI_COMMIT_SHA:-$(git rev-parse HEAD)}"

echo "==> pts-build: ${VERSION} (${SHA_SHORT})"

# ── d3ci42 SSH setup ──
SSH_KEY=".deploy-ssh/id_ed25519"
mkdir -p .deploy-ssh
echo "$DEPLOY_SSH_KEY" > "$SSH_KEY"
printf '\n' >> "$SSH_KEY"
chmod 600 "$SSH_KEY"
SSH="ssh -i $SSH_KEY -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR"
ssh_cmd() { $SSH "root@${SERVER_HOST}" "$@"; }

# ── pentest-dev SSH setup ──
PTS_KEY=".deploy-ssh/pts-build-key"
echo "$PTS_BUILD_SSH_KEY" > "$PTS_KEY"
printf '\n' >> "$PTS_KEY"
chmod 600 "$PTS_KEY"
PTS_SSH="ssh -i $PTS_KEY -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR"

# ── Start pentest-dev-vm ──
echo ""
echo "==> Starting ${PENTEST_VM}..."
gcloud compute instances start "${PENTEST_VM}" \
    --zone="${PENTEST_ZONE}" --project="${PENTEST_PROJECT}" --quiet
PENTEST_IP=$(gcloud compute instances describe "${PENTEST_VM}" \
    --zone="${PENTEST_ZONE}" --project="${PENTEST_PROJECT}" \
    --format="value(networkInterfaces[0].accessConfigs[0].natIP)")
echo "    IP: ${PENTEST_IP}"

# Always stop pentest-dev on exit (success or failure)
trap "echo '==> Stopping ${PENTEST_VM}...' && \
      gcloud compute instances stop '${PENTEST_VM}' \
          --zone='${PENTEST_ZONE}' --project='${PENTEST_PROJECT}' --quiet 2>/dev/null || true" EXIT

# Wait for SSH (up to 2 min)
echo "==> Waiting for SSH on pentest-dev..."
for i in $(seq 1 12); do
    $PTS_SSH "root@${PENTEST_IP}" true 2>/dev/null && echo "    SSH ready (attempt ${i})" && break
    [ "$i" -eq 12 ] && echo "ERROR: pentest-dev SSH timeout" && exit 1
    sleep 10
done

# ── Compile on pentest-dev-vm ──
WORK_DIR="/tmp/pts-build-${CI_PIPELINE_NUMBER:-0}"
echo ""
echo "==> Compiling on ${PENTEST_VM} (GCS cache warm)..."
$PTS_SSH "root@${PENTEST_IP}" \
    VERSION="${VERSION}" COMMIT_SHA="${COMMIT_SHA}" WORK_DIR="${WORK_DIR}" bash << 'REMOTE'
set -euo pipefail

export PATH="/usr/local/go/bin:$PATH"
BUILD_CACHE_BUCKET="gs://ci-runners-de-build-cache"
GOCACHE="${HOME}/.cache/go-build"
GOMODCACHE="${HOME}/go/pkg/mod"

# Restore GCS build cache (#851)
echo "  restoring GCS cache..."
mkdir -p "${GOCACHE}" "${GOMODCACHE}"
gsutil -m -q rsync -r "${BUILD_CACHE_BUCKET}/go-build/" "${GOCACHE}/" 2>/dev/null && \
    echo "  go-build: $(du -sh "${GOCACHE}" 2>/dev/null | cut -f1)" || echo "  go-build: cold"
gsutil -m -q rsync -r "${BUILD_CACHE_BUCKET}/go-mod/" "${GOMODCACHE}/" 2>/dev/null && \
    echo "  go-mod: $(du -sh "${GOMODCACHE}" 2>/dev/null | cut -f1)" || echo "  go-mod: cold"

# Clone at exact commit
rm -rf "${WORK_DIR}"
git clone --quiet https://github.com/Peregrine-Technology-Systems/woodpecker.git "${WORK_DIR}"
cd "${WORK_DIR}"
git checkout --quiet "${COMMIT_SHA}"

echo "  building web UI..."
cd web && pnpm install --frozen-lockfile >/dev/null 2>&1 && pnpm build >/dev/null 2>&1
echo "  web UI: $(ls dist/ | wc -l) files"
cd ..

mkdir -p bin
echo "  compiling woodpecker-server (CGO=1)..."
CGO_ENABLED=1 go build \
    -ldflags "-s -w -X go.woodpecker-ci.org/woodpecker/v3/version.Version=${VERSION}" \
    -o bin/woodpecker-server ./cmd/server
echo "  server: $(du -h bin/woodpecker-server | cut -f1)"

echo "  compiling woodpecker-agent (CGO=0)..."
CGO_ENABLED=0 go build \
    -ldflags "-s -w -X go.woodpecker-ci.org/woodpecker/v3/version.Version=${VERSION}" \
    -o bin/woodpecker-agent ./cmd/agent
echo "  agent: $(du -h bin/woodpecker-agent | cut -f1)"

# Save GCS build cache
echo "  saving GCS cache..."
gsutil -m -q rsync -r "${GOCACHE}/" "${BUILD_CACHE_BUCKET}/go-build/" 2>/dev/null && \
    echo "  go-build saved" || echo "  ⚠️  go-build save failed (non-fatal)"
gsutil -m -q rsync -r "${GOMODCACHE}/" "${BUILD_CACHE_BUCKET}/go-mod/" 2>/dev/null && \
    echo "  go-mod saved" || echo "  ⚠️  go-mod save failed (non-fatal)"
REMOTE

# ── rsync binaries back to CI agent ──
echo ""
echo "==> Fetching binaries from pentest-dev..."
mkdir -p bin
rsync -az \
    -e "ssh -i $PTS_KEY -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR" \
    "root@${PENTEST_IP}:${WORK_DIR}/bin/" bin/
echo "    server: $(du -h bin/woodpecker-server | cut -f1)  agent: $(du -h bin/woodpecker-agent | cut -f1)"
# pentest-dev VM stopped by EXIT trap here

# ── Deploy server to d3ci42 ──
echo ""
echo "==> Deploying ${VERSION} to ${SERVER_HOST}..."

# Capture previous release for rollback
PREVIOUS=$(ssh_cmd "readlink -f ${CURRENT_LINK} 2>/dev/null | xargs basename 2>/dev/null || echo ''" || echo "")
echo "    Previous: ${PREVIOUS:-<none>}"

# Rsync binary
ssh_cmd "mkdir -p ${RELEASES_DIR}/${VERSION}"
rsync -az --checksum \
    -e "ssh -i $SSH_KEY -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR" \
    bin/woodpecker-server "root@${SERVER_HOST}:${RELEASES_DIR}/${VERSION}/woodpecker-server"
ssh_cmd "chmod 755 ${RELEASES_DIR}/${VERSION}/woodpecker-server"

# Checksum verify
LOCAL_SHA=$(sha256sum bin/woodpecker-server | awk '{print $1}')
REMOTE_SHA=$(ssh_cmd "sha256sum ${RELEASES_DIR}/${VERSION}/woodpecker-server | awk '{print \$1}'")
if [ "$LOCAL_SHA" != "$REMOTE_SHA" ]; then
    echo "ERROR: checksum mismatch after rsync"
    exit 1
fi
echo "    Checksum verified: ${LOCAL_SHA:0:16}..."

# Atomic symlink + restart
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

# Prune old releases (keep KEEP_RELEASES)
ssh_cmd "
    find ${RELEASES_DIR} -maxdepth 1 -mindepth 1 -type d | sort -r | tail -n +$((KEEP_RELEASES + 1)) | while read -r dir; do
        current=\$(readlink -f ${CURRENT_LINK} 2>/dev/null || echo '')
        [ \"\$dir\" != \"\$current\" ] && rm -rf \"\$dir\" && echo \"Pruned: \$dir\"
    done
" 2>/dev/null || true

echo "✅ ${VERSION} deployed to ${SERVER_HOST}"

# ─────────────────────────────────────────────────────────────────────────────
# Phase 3: GitHub Release + binary assets + builder VM wake
# ─────────────────────────────────────────────────────────────────────────────
REPO_FULL="Peregrine-Technology-Systems/woodpecker"
GH_API="https://api.github.com"
GH_AUTH="Authorization: Bearer ${GH_TOKEN:-}"

if [ -z "${GH_TOKEN:-}" ]; then
    echo ""
    echo "==> Skipping GH Release + builder wake: GH_TOKEN not set"
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
RELEASE_BODY="Automated release from pts-build pipeline ${CI_PIPELINE_NUMBER:-?}.\n\nBinaries: \`woodpecker-server-linux-amd64\` and \`woodpecker-agent-linux-amd64\` attached as release assets.\nDeployed to d3ci42 via native rsync (woodpecker#57)."
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

# Upload helper — idempotent (delete existing asset first)
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
cat > "${JOB_FILE}" <<JOB
{"request_id":"${REQUEST_ID}","wp_version":"${VERSION}","scaler_version":"","triggered_by":"woodpecker-pts-build-${CI_PIPELINE_NUMBER:-?}","created_at":"${NOW_ISO}"}
JOB

if gsutil -q cp "${JOB_FILE}" "gs://${BUILDER_BUCKET}/jobs/${REQUEST_ID}.json" 2>/dev/null; then
    echo "    Wrote job: ${REQUEST_ID}"
    gcloud --quiet compute instances start "${BUILDER_VM}" --zone="${BUILDER_ZONE}" --project="${BUILDER_PROJECT}" 2>/dev/null && \
        echo "    Started ${BUILDER_VM}" || echo "    ⚠️  Could not start builder VM — job written, will be picked up next wake"
else
    echo "    ⚠️  Could not write job file — non-blocking"
fi
rm -f "${JOB_FILE}"

echo ""
echo "✅ pts-build ${VERSION} complete: deployed + GH Release + agent binary + builder wake"
