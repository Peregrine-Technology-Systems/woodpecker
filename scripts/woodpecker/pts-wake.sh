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

# #353: pts-build-vm now lives in peregrine-production. The legacy project is
# under decommission, and its agent identity can never be granted anything on a
# PP resource under the org's DRS policy — so this move required a PP-native
# identity to make the create call, not a cross-project grant. See the auth block
# below and infra#5682/#5698.
PTS_BUILD_PROJECT="${PTS_BUILD_PROJECT:-peregrine-production}"
PTS_BUILD_VM="pts-build-vm"
WP_API="https://d3ci42.peregrinetechsys.net"

# Image: PP has no family literally named `ci-agent` — it is `ci-agent-base-base`
# (verified live; head carries the go the build needs). A family lookup is fine
# for `gcloud compute instances create`: the ForceNew hazard that blocked adding
# a family in terraform (infra#5688/#5691) applies to terraform-managed image
# resources and instance templates, not to this call. Override either value to
# pin a specific image for a one-off.
PTS_BUILD_IMAGE_FAMILY="${PTS_BUILD_IMAGE_FAMILY:-ci-agent-base-base}"
PTS_BUILD_IMAGE_PROJECT="${PTS_BUILD_IMAGE_PROJECT:-peregrine-production}"

# Auth: the wake step runs on d3ci42-local under the LEGACY ambient identity,
# which cannot create anything in PP. infra's deploy rsyncs the mint script for
# the dedicated pts-build-wake identity to /opt/woodpecker; we exchange it for a
# short-lived token and hand that to gcloud via CLOUDSDK_AUTH_ACCESS_TOKEN_FILE,
# which every gcloud call below AND in lib/wake-helpers.sh then picks up. Using
# the env var rather than a per-call --access-token-file flag is deliberate: a
# call site that missed the flag would silently fall back to the ambient identity,
# which is precisely the failure this change exists to remove.
PTS_WAKE_MINT_SCRIPT="${PTS_WAKE_MINT_SCRIPT:-/opt/woodpecker/pts-build-wake-mint-token.sh}"

# Fail-safe helpers — get_vm_owner_pipeline / get_pipeline_status branch on exit
# status so an undetermined owner/status never falls through to a destructive
# delete (#3089 class).
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/woodpecker/lib/wake-helpers.sh
source "${SCRIPT_DIR}/lib/wake-helpers.sh"

# ── Authenticate as pts-build-wake, or abort (#353) ──
# Fail loud rather than proceeding: an unauthenticated run inherits the legacy
# ambient identity and 403s in every zone, which the fallback loop then reports
# as capacity exhaustion (#369) — a permissions-shaped symptom for an
# authentication-shaped cause. Two reverts came from exactly that confusion.
PTS_WAKE_TOKEN_FILE="$(mktemp)"
PTS_WAKE_CREATE_ERR="$(mktemp)"
trap 'rm -f "${PTS_WAKE_TOKEN_FILE}" "${PTS_WAKE_CREATE_ERR}"' EXIT
if ! mint_wake_token "${PTS_WAKE_MINT_SCRIPT}" "${PTS_WAKE_TOKEN_FILE}"; then
    echo "ERROR: could not mint a pts-build-wake token; refusing to fall back to the" >&2
    echo "       ambient (legacy) identity, which cannot create instances in" >&2
    echo "       ${PTS_BUILD_PROJECT} and would 403 in every zone (#353)." >&2
    echo "       Expected mint script at ${PTS_WAKE_MINT_SCRIPT} — infra's" >&2
    echo "       deploy-woodpecker-server.sh places it there on every deploy." >&2
    exit 1
fi
export CLOUDSDK_AUTH_ACCESS_TOKEN_FILE="${PTS_WAKE_TOKEN_FILE}"
echo "==> Authenticated as pts-build-wake for ${PTS_BUILD_PROJECT}"

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
