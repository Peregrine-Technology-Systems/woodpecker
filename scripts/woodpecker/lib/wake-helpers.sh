#!/usr/bin/env bash
# wake-helpers.sh — sourceable helpers for pts-wake.sh.
#
# [pts] Hardened against the silent-OK / fail-open class documented in
# peregrine-infrastructure#3089 (a `2>/dev/null … || true` swallow made a
# backend error indistinguishable from a benign empty/absent result, so a guard
# proceeded as if the real-world state were safe). Both functions below key
# their decision on the command's EXIT STATUS, never on output-emptiness, so an
# undetermined owner/status can never fall through to the destructive
# `gcloud … instances delete` in the wake mutex (#120).
#
# (woodpecker-deploy.sh's pending-marker / token-rotation helpers used to live
# here too; that script is owned and deployed by peregrine-infrastructure
# (woodpecker-server/woodpecker-deploy.sh), not this fork — the fork's copy was a
# non-deployed fossil and has been removed. The pending-marker fail-close fix was
# filed against infra. See docs/ARCHITECTURE.md.)
#
# This file has NO top-level side effects so it can be sourced by bats with
# gcloud/curl on PATH replaced by stubs.

# get_vm_owner_pipeline VM ZONE PROJECT
#   Reads the pts-build-pipeline owner id from a running VM's metadata.
#   stdout : owner id (may be empty/"0" when genuinely unset)
#   return : 0 = read succeeded (value on stdout)
#            2 = read error (could NOT determine — caller must NOT treat as unowned)
#
#   Replaces `OWNER=$(gcloud … || echo "")`, where a transient gcloud error
#   produced an empty owner that fell straight through to `gcloud … delete`,
#   destroying a VM that might be actively compiling another build (#3089 shape,
#   destructive). The fail-safe rule: when ownership can't be determined, the
#   caller keeps the VM.
get_vm_owner_pipeline() {
  local vm="$1" zone="$2" project="$3" val
  if ! val="$(gcloud compute instances describe "${vm}" \
        --zone="${zone}" --project="${project}" \
        --format="value(metadata.items[pts-build-pipeline])" 2>/dev/null)"; then
    return 2
  fi
  printf '%s' "${val}"
  return 0
}

# get_pipeline_status WP_API TOKEN PIPELINE
#   Reads a Woodpecker pipeline's status.
#   stdout : status string (e.g. running/pending/success/failure)
#   return : 0 = read + parsed ok, 2 = error (HTTP/parse failure — undetermined)
#
#   Replaces `… | python3 … || echo "unknown"`, where any HTTP/parse error
#   became "unknown" and "unknown" fell through to delete. Now an error is a
#   distinct exit code so the caller can refuse to delete on an undetermined
#   status rather than assuming the build is finished.
get_pipeline_status() {
  local wp_api="$1" token="$2" pipeline="$3" body status
  if ! body="$(curl -sf --max-time 5 \
        -H "Authorization: Bearer ${token}" \
        "${wp_api}/api/repos/13/pipelines/${pipeline}" 2>/dev/null)"; then
    return 2
  fi
  if ! status="$(printf '%s' "${body}" \
        | python3 -c "import sys,json; print(json.load(sys.stdin)['status'])" 2>/dev/null)"; then
    return 2
  fi
  printf '%s' "${status}"
  return 0
}
