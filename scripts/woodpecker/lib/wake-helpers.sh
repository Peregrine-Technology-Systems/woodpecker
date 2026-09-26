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

# mint_wake_token MINT_SCRIPT OUT_FILE
#   Mints a GCP access token for the dedicated pts-build-wake identity and
#   leaves it in OUT_FILE (mode 600) for gcloud's --access-token-file /
#   CLOUDSDK_AUTH_ACCESS_TOKEN_FILE.
#   return : 0 = OUT_FILE holds a usable, non-blank token
#            2 = could not obtain one — the caller MUST abort
#
#   #353/#354/#355: pts-wake.sh used to authenticate not at all, inheriting
#   whatever identity was ambient on the host. That is the LEGACY project's
#   agent, which under the org's DRS policy can never be granted anything on a
#   peregrine-production resource — so the repoint 403'd in all 11 zones, and
#   the zone-fallback loop reported it as capacity exhaustion (#369).
#
#   The load-bearing property is that failure returns 2 rather than letting the
#   caller proceed. A silent fallback to the ambient identity reproduces exactly
#   that 403 and presents as a permissions problem rather than an authentication
#   one — which is how it cost two reverts.
#
#   Note the deliberate absence of 2>/dev/null on the mint invocation: infra's
#   bakery runbook records "NEVER 2>/dev/null a mint; a silent mint failure looks
#   like a permission problem three commands later". The mint's own stdout is
#   routed to stderr so its diagnostics stay visible without polluting a
#   command-substitution capture of this function.
mint_wake_token() {
  local mint="$1" out="$2"

  if [ ! -x "${mint}" ]; then
    echo "mint_wake_token: mint script not executable: ${mint}" >&2
    return 2
  fi

  # Create the destination 600 BEFORE the mint writes, so the token never
  # exists briefly at the ambient umask.
  if ! : > "${out}" 2>/dev/null || ! chmod 600 "${out}" 2>/dev/null; then
    echo "mint_wake_token: cannot create token file: ${out}" >&2
    return 2
  fi

  if ! "${mint}" "${out}" >&2; then
    echo "mint_wake_token: mint failed (see above): ${mint}" >&2
    return 2
  fi

  # An exit-0 mint that wrote nothing usable is the silent-OK shape: the failure
  # would otherwise surface as a 401 from gcloud several commands later.
  if [ -z "$(tr -d '[:space:]' < "${out}" 2>/dev/null)" ]; then
    echo "mint_wake_token: mint exited 0 but wrote no token: ${out}" >&2
    return 2
  fi

  return 0
}

# zone_failure_is_retryable OUTPUT
#   Decides whether a failed `gcloud compute instances create` is worth trying in
#   another zone.
#   return : 0 = genuine per-zone stockout — try the next zone
#            1 = anything else — abort now and report the real reason
#
#   #369 residue: the zone-fallback loop printed "capacity unavailable" for every
#   failure and "all zones at capacity" after eleven of them — a cause the script
#   never observed. Pipeline #581's permanent 403 was retried in all 11 zones and
#   summarised as a stockout, which sent the diagnosis in the wrong direction
#   entirely. Quota is deliberately NOT retryable: it is global, so switching
#   zones provably cannot help, and a previous bake wedge was diagnosed only
#   after 11 pointless attempts.
#
#   Unrecognised output is NOT retryable, on purpose. An unknown error reported
#   once with its own text is strictly more useful than the same error eleven
#   times relabelled as capacity.
zone_failure_is_retryable() {
  local out="$1"

  # Genuine per-zone capacity — the only case another zone can fix.
  case "${out}" in
    *ZONE_RESOURCE_POOL_EXHAUSTED*) return 0 ;;
    *"does not have enough resources"*) return 0 ;;
    *"resource pool exhausted"*) return 0 ;;
  esac

  return 1
}
