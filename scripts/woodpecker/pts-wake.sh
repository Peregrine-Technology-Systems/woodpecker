#!/usr/bin/env bash
# pts-wake.sh — provision pts-build-vm for woodpecker compilation (#1669).
# Ephemeral pattern: always creates fresh from ci-agent family image so every
# build gets the latest agent binary and toolchain. No image staleness, no
# stale agent.conf, no manual taint/recreate cycle.
#
# Zone fallback (#150): tries each of the 11 agent zones in sequence until one
# has e2-standard-8 capacity. Zone used is stored in VM metadata so
# pts-cleanup.sh can delete the VM in the correct zone.
#
# States:
#   NOT_FOUND / TERMINATED → create fresh (zone fallback)
#   RUNNING + active owner  → abort (concurrent build in progress)
#   RUNNING + stale owner   → delete + recreate
#
# Build cache lives in GCS — no persistent disk needed.
set -euo pipefail

# #353: which project/image/identity set to build in. DEFAULT IS `legacy` —
# current behaviour — so landing this change is inert. pts-build.yaml is
# `event: manual` restricted to `branch: main` and runs this script from the
# checked-out workspace, so a change cannot be exercised from a PR branch and
# merging arms the next bake; with the create path still unproven, the default
# must be what already works. One switch selects the whole coherent set (see
# resolve_build_target) rather than three independent knobs that could combine
# incorrectly. Set PTS_BUILD_TARGET=peregrine-production on a manual run to
# exercise the cutover; flipping the default is a separate one-line change once a
# real bake has passed.
# (resolved below, AFTER lib/wake-helpers.sh is sourced — it owns the default.)
PTS_WAKE_MINT_SCRIPT="${PTS_WAKE_MINT_SCRIPT:-/opt/woodpecker/pts-build-wake-mint-token.sh}"

PTS_BUILD_VM="pts-build-vm"
WP_API="https://d3ci42.peregrinetechsys.net"

# Fail-safe helpers — get_vm_owner_pipeline / get_pipeline_status branch on exit
# status so an undetermined owner/status never falls through to a destructive
# delete (#3089 class).
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/woodpecker/lib/wake-helpers.sh
source "${SCRIPT_DIR}/lib/wake-helpers.sh"

# ── Resolve the build target, then authenticate for it (#353) ──
# PTS_BUILD_TARGET_DEFAULT is defined by lib/wake-helpers.sh, so this must come
# AFTER the source above. It was briefly above it, and under `set -u` that made
# every default run die with "unbound variable" — caught only because the test
# exercised the no-env case; the two tests that set the variable explicitly never
# evaluated the default and passed happily.
PTS_BUILD_TARGET="${PTS_BUILD_TARGET:-${PTS_BUILD_TARGET_DEFAULT}}"
if ! resolve_build_target "${PTS_BUILD_TARGET}"; then
    echo "ERROR: unknown PTS_BUILD_TARGET='${PTS_BUILD_TARGET}'." >&2
    echo "       Valid: legacy | peregrine-production. Refusing to guess — a typo" >&2
    echo "       that silently fell back to legacy would make a green pipeline read" >&2
    echo "       as a successful cutover (#353)." >&2
    exit 1
fi
PTS_BUILD_PROJECT="${BUILD_PROJECT}"
PTS_BUILD_IMAGE_FAMILY="${BUILD_IMAGE_FAMILY}"
PTS_BUILD_IMAGE_PROJECT="${BUILD_IMAGE_PROJECT}"
echo "==> target=${PTS_BUILD_TARGET} project=${PTS_BUILD_PROJECT} image=${PTS_BUILD_IMAGE_FAMILY} auth=${BUILD_AUTH}"

# Both temp files are created unconditionally, even though the token file is only
# USED in peregrine-production mode. That keeps the cleanup trap operating on two
# real paths: a conditional `[ -n "$x" ] && rm ...` returns non-zero when the test
# fails, which under `set -e` is the same chain-clobbering shape this repo already
# tripped over in #369, and an empty variable inside an `rm` argument list is a
# footgun however carefully it is guarded.
PTS_WAKE_CREATE_ERR="$(mktemp)"
PTS_WAKE_TOKEN_FILE="$(mktemp)"
chmod 600 "${PTS_WAKE_TOKEN_FILE}"
trap 'rm -f "${PTS_WAKE_CREATE_ERR}" "${PTS_WAKE_TOKEN_FILE}"' EXIT

if [ "${BUILD_AUTH}" = "pts-build-wake" ]; then
    # Fail loud rather than proceeding: an unauthenticated run inherits the legacy
    # ambient identity, which can see nothing in peregrine-production (verified —
    # 0 instances visible vs 8 with a minted token), so it 403s in every zone and
    # the fallback loop used to report that as capacity exhaustion (#369). Two
    # reverts came from exactly that confusion.
    if ! mint_wake_token "${PTS_WAKE_MINT_SCRIPT}" "${PTS_WAKE_TOKEN_FILE}"; then
        echo "ERROR: could not mint a pts-build-wake token; refusing to fall back to" >&2
        echo "       the ambient (legacy) identity, which cannot create instances in" >&2
        echo "       ${PTS_BUILD_PROJECT} (#353)." >&2
        echo "       Expected mint script at ${PTS_WAKE_MINT_SCRIPT} — infra's" >&2
        echo "       deploy-woodpecker-server.sh places it there on every deploy." >&2
        exit 1
    fi
    # The env var covers every gcloud call in this script AND in the sourced
    # helpers. A per-call --access-token-file flag would leave a missed call site
    # falling back silently to the ambient identity — the failure being removed.
    # Verified on the host that the env var is honoured (8 PP instances visible
    # with a good token, 0 without).
    export CLOUDSDK_AUTH_ACCESS_TOKEN_FILE="${PTS_WAKE_TOKEN_FILE}"
    echo "==> Authenticated as pts-build-wake for ${PTS_BUILD_PROJECT}"
fi

# Same zone list used by ci-image-builder bootstrap.sh (peregrine-infrastructure PR #1665)
ZONE_COUNT=11
ZONE_LIST="us-central1-a us-central1-b us-east1-b us-east1-c us-east1-d us-west1-a us-west1-b us-west1-c us-east4-a us-east4-b us-south1-b"

# ── Concurrent-run guard ──
# Describe without --zone so we find the VM regardless of which zone it's in.
DESCRIBE=$(gcloud compute instances list \
    --project="${PTS_BUILD_PROJECT}" \
    --filter="name=${PTS_BUILD_VM}" \
    --format="value(status,zone)" 2>/dev/null | head -1)
STATUS=$(echo "${DESCRIBE}" | awk '{print $1}')
CURRENT_ZONE=$(echo "${DESCRIBE}" | awk '{print $2}' | awk -F/ '{print $NF}')
STATUS="${STATUS:-NOT_FOUND}"

if [ "${STATUS}" = "RUNNING" ] && [ -n "${CURRENT_ZONE}" ]; then
    # Fail-safe mutex (#120 / #3089): only delete the running VM when we can
    # POSITIVELY determine it is stale/unowned. If ownership or owner-status
    # can't be determined (gcloud/API error), keep the VM — never destroy what
    # we can't prove is safe to destroy.
    if ! OWNER_PIPELINE=$(get_vm_owner_pipeline "${PTS_BUILD_VM}" "${CURRENT_ZONE}" "${PTS_BUILD_PROJECT}"); then
        echo "==> ${PTS_BUILD_VM} RUNNING in ${CURRENT_ZONE} but owner metadata is UNREADABLE — refusing to delete (fail-safe, #120). Aborting wake; TTL reaper (#228) backstops a truly-stale VM."
        exit 0
    fi
    if [ -n "${OWNER_PIPELINE}" ] && [ "${OWNER_PIPELINE}" != "0" ]; then
        if ! OWNER_STATUS=$(get_pipeline_status "${WP_API}" "${WOODPECKER_API_TOKEN}" "${OWNER_PIPELINE}"); then
            echo "==> ${PTS_BUILD_VM} owned by #${OWNER_PIPELINE} but its status is UNDETERMINED (API/parse error) — refusing to delete (fail-safe, #120). Aborting wake."
            exit 0
        fi
        echo "==> ${PTS_BUILD_VM} RUNNING in ${CURRENT_ZONE} — owner=#${OWNER_PIPELINE} status=${OWNER_STATUS}"
        if [ "${OWNER_STATUS}" = "running" ] || [ "${OWNER_STATUS}" = "pending" ]; then
            echo "    Active pipeline #${OWNER_PIPELINE} is compiling — aborting (#120)."
            exit 0
        fi
    fi
    echo "==> Stale or unowned VM in ${CURRENT_ZONE} — deleting before recreate..."
    gcloud compute instances delete "${PTS_BUILD_VM}" \
        --zone="${CURRENT_ZONE}" --project="${PTS_BUILD_PROJECT}" --quiet 2>/dev/null || true
fi

# ── Create fresh from latest ci-agent family image — zone fallback (#150) ──
echo "==> Creating ${PTS_BUILD_VM} from ci-agent family (pts.${CI_PIPELINE_NUMBER:-0})..."
CREATED_ZONE=""
for ZONE in ${ZONE_LIST}; do
    # #250: pin pts-build-vm to the buildkite-network VPC (NOT the default
    # network). Every scaler-managed agent runs on buildkite-network; a VM left
    # on the default network registers but its agent WebSocket to d3ci42 flaps
    # with `close 1006 (unexpected EOF)` (Mode-C), so it never holds a fresh
    # last_contact and the registration poll below times out at 180s (the bake
    # wedge diagnosed for pts.455/456). Matching the working agents' VPC is the
    # cut-at-source fix. Subnet is per-region: us-central1 uses the legacy
    # `buildkite-network-subnet-0`, every other region uses
    # `buildkite-network-<region>` (`gcloud compute networks subnets list
    # --network=buildkite-network`).
    REGION="${ZONE%-*}"
    if [ "${REGION}" = "us-central1" ]; then
        SUBNET="buildkite-network-subnet-0"
    else
        SUBNET="buildkite-network-${REGION}"
    fi
    echo "    Trying zone ${ZONE} (network buildkite-network/${SUBNET})..."
    if gcloud compute instances create "${PTS_BUILD_VM}" \
            --project="${PTS_BUILD_PROJECT}" \
            --zone="${ZONE}" \
            --machine-type=e2-standard-8 \
            --image-family="${PTS_BUILD_IMAGE_FAMILY}" \
            --image-project="${PTS_BUILD_IMAGE_PROJECT}" \
            --boot-disk-size=50GB \
            --boot-disk-type=pd-ssd \
            --service-account="ci-agent@${PTS_BUILD_PROJECT}.iam.gserviceaccount.com" \
            --scopes=cloud-platform \
            --network="buildkite-network" \
            --subnet="${SUBNET}" \
            --tags=pts-build,woodpecker-agent \
            --metadata="agent-label=pts-build,pts-build-pipeline=${CI_PIPELINE_NUMBER:-0},pts-build-zone=${ZONE}" \
            --no-restart-on-failure \
            --maintenance-policy=MIGRATE \
            --quiet 2>&1 | tee "${PTS_WAKE_CREATE_ERR}"; then
        CREATED_ZONE="${ZONE}"
        echo "    VM created in ${CREATED_ZONE} from latest ci-agent image"
        # TTL backstop — reaper stops the VM within 2h if the pipeline
        # doesn't delete it first (#228).
        gcloud compute instances add-labels "${PTS_BUILD_VM}" \
            --zone="${CREATED_ZONE}" --project="${PTS_BUILD_PROJECT}" \
            --labels="ttl-override-min=120" 2>/dev/null || true
        echo "    TTL label set (120 min backstop)"
        break
    fi
    # #369: report the cause we actually observed, and only advance the loop for a
    # failure another zone could fix. Anything else — auth, permissions, quota
    # (global), a missing image — fails here with its real text instead of being
    # relabelled "capacity" eleven times over.
    PTS_WAKE_LAST_ERR="$(cat "${PTS_WAKE_CREATE_ERR}" 2>/dev/null || true)"
    if ! zone_failure_is_retryable "${PTS_WAKE_LAST_ERR}"; then
        echo "ERROR: ${PTS_BUILD_VM} create failed in ${ZONE} for a reason another zone cannot fix:" >&2
        echo "${PTS_WAKE_LAST_ERR}" | tail -5 >&2
        exit 1
    fi
    echo "    WARN: ${ZONE} genuinely out of capacity — trying next zone"
done

if [ -z "${CREATED_ZONE}" ]; then
    echo "ERROR: could not create ${PTS_BUILD_VM} in any of the ${ZONE_COUNT} zones tried." >&2
    echo "       Every attempt was a genuine capacity failure. Last reported reason:" >&2
    echo "${PTS_WAKE_LAST_ERR:-<none captured>}" | tail -5 >&2
    exit 1
fi

# ── Poll until agent registers ──
# woodpecker-agent-config.service runs on boot, writes agent.env, then
# woodpecker-agent.service starts — allow ~3 min for this sequence.
echo "==> Waiting for pts-build-vm agent to register with Woodpecker..."
for i in $(seq 1 36); do
    FOUND=$(curl -sf --max-time 5 "${WP_API}/api/agents" \
        -H "Authorization: Bearer ${WOODPECKER_API_TOKEN}" 2>/dev/null | \
        python3 -c "
import sys,json,time
agents=json.load(sys.stdin)
now=int(time.time())
found=[a for a in agents if a.get('name','').startswith('pts-build-vm') and now-a.get('last_contact',0)<60]
print(len(found))
" 2>/dev/null || echo 0)
    if [ "${FOUND:-0}" -gt 0 ]; then
        echo "    pts-build-vm agent registered (attempt ${i})"
        exit 0
    fi
    echo "    waiting... (attempt ${i}/36, $((i*5))s elapsed)"
    sleep 5
done
echo "ERROR: pts-build-vm agent did not register within 180s"
exit 1
