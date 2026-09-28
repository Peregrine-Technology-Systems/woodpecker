#!/usr/bin/env bash
# pts-cleanup.sh — delete pts-build-vm after compile completes.
# Ephemeral pattern (#1669): VM is deleted so next build always creates fresh
# from latest ci-agent family image. Build cache persists in GCS.
# Zone is read from VM metadata (pts-build-zone) since zone fallback (#150)
# may have created the VM in any of the 11 agent zones.
set -euo pipefail

# #380: resolve the same coherent set pts-wake.sh does. This script hardcoded
# the legacy project, so after #376 moved the VM to peregrine-production the
# delete ran against a project the VM was not in, found nothing, and reported
# success — leaving an e2-standard-8 running (#601). A VM created by one target
# must never be deleted from another.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/woodpecker/lib/wake-helpers.sh
source "${SCRIPT_DIR}/lib/wake-helpers.sh"

PTS_BUILD_TARGET="${PTS_BUILD_TARGET:-${PTS_BUILD_TARGET_DEFAULT}}"
if ! resolve_build_target "${PTS_BUILD_TARGET}"; then
    echo "ERROR: unknown PTS_BUILD_TARGET='${PTS_BUILD_TARGET}'. Valid: legacy | peregrine-production." >&2
    echo "       Refusing to guess which project to delete from (#380)." >&2
    exit 1
fi
PTS_BUILD_PROJECT="${BUILD_PROJECT}"
PTS_BUILD_VM="pts-build-vm"
MY_PIPELINE="${CI_PIPELINE_NUMBER:-0}"
echo "==> cleanup target=${PTS_BUILD_TARGET} project=${PTS_BUILD_PROJECT}"

# Three states, kept apart. The old form collapsed them into "not found" + exit 0,
# which is how #601's orphan went unnoticed: the list was looking in the wrong
# project, so "no rows" was true and meaningless.
DESCRIBE=""
find_rc=0
DESCRIBE="$(find_build_vm "${PTS_BUILD_PROJECT}" "${PTS_BUILD_VM}")" || find_rc=$?

if [ "${find_rc}" -eq 2 ]; then
    echo "ERROR: could not determine whether ${PTS_BUILD_VM} exists in ${PTS_BUILD_PROJECT}." >&2
    echo "       The list itself failed, so absence is UNKNOWN, not proven. Refusing to" >&2
    echo "       report success on an undetermined state (#380)." >&2
    exit 1
fi

if [ "${find_rc}" -eq 1 ]; then
    echo "==> ${PTS_BUILD_VM} genuinely absent in ${PTS_BUILD_PROJECT} — nothing to delete"
    exit 0
fi

PTS_BUILD_ZONE=$(echo "${DESCRIBE}" | awk '{print $1}' | awk -F/ '{print $NF}')
OWNER=$(echo "${DESCRIBE}" | awk '{print $2}')

echo "==> Mutex check: my pipeline=#${MY_PIPELINE} owner=#${OWNER:-unknown} zone=${PTS_BUILD_ZONE}"

if [ -n "${OWNER}" ] && [ "${OWNER}" -gt "${MY_PIPELINE}" ] 2>/dev/null; then
    echo "    VM owned by newer pipeline #${OWNER} — skipping delete (superseded)"
    exit 0
fi

echo "==> Deleting ${PTS_BUILD_VM} in ${PTS_BUILD_ZONE} (ephemeral — fresh image on next build)..."
if delete_build_vm "${PTS_BUILD_VM}" "${PTS_BUILD_ZONE}" "${PTS_BUILD_PROJECT}"; then
    echo "    deleted — absence confirmed by a second look, not by the delete's own exit code"
    exit 0
fi

# We FOUND a VM and it is still there (or we cannot prove otherwise). That is a
# leak, and a leak that reports success is how #601 cost a running e2-standard-8.
echo "ERROR: ${PTS_BUILD_VM} was present in ${PTS_BUILD_PROJECT} and is NOT confirmed gone." >&2
echo "       Failing loudly rather than exiting 0 on an unconfirmed delete (#380)." >&2
echo "       Check for an orphaned VM in ${PTS_BUILD_PROJECT} before re-running." >&2
exit 1
