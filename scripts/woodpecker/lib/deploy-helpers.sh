#!/usr/bin/env bash
# deploy-helpers.sh — sourceable helpers for woodpecker-deploy.sh.
#
# [pts] Hardened against the silent-OK / fail-open class documented in
# peregrine-infrastructure#3089 (a `2>/dev/null … || true` swallow made a
# backend error indistinguishable from a benign empty/absent result, so a guard
# proceeded as if the real-world state were safe). Every function below keys its
# decision on the command's EXIT STATUS, never on output-emptiness, and
# distinguishes "genuinely absent" (quiet, common) from "could not determine"
# (loud, never silently treated as absent).
#
# This file has NO top-level side effects so it can be sourced by bats with
# gcloud/gsutil/curl on PATH replaced by stubs.

# read_pending_marker BUCKET
#   Reads the GCS pending-deploy marker.
#   stdout : marker content (only when present)
#   return : 0 = present (content on stdout)
#            1 = genuinely absent  (no marker — the common idle case, quiet)
#            2 = backend error     (could NOT determine — caller must alert, not skip)
#
#   The old inline form was `PENDING=$(gsutil -q cat … 2>/dev/null || echo "")`
#   then `[ -z "$PENDING" ] && exit 0`. A persistent auth/transient gsutil error
#   was byte-identical to "no pending deploy", so deploys could silently never
#   fire while the timer reported clean on every tick. We classify instead:
#   absent stays quiet; a real error is surfaced loudly.
read_pending_marker() {
  local bucket="$1" out err rc
  err="$(mktemp)"
  out="$(gsutil -q cat "${bucket}/pending" 2>"${err}")"
  rc=$?

  if [ "${rc}" -eq 0 ]; then
    rm -f "${err}"
    printf '%s' "${out}"
    return 0
  fi

  # rc != 0: distinguish "object not found" (absent) from any other failure
  # (auth, network, quota, …). Only a positive not-found signal is treated as
  # absent; anything ambiguous fails toward "error" so it can never masquerade
  # as idle.
  if grep -qiE 'no url|no such|matched no objects|not found|404' "${err}"; then
    rm -f "${err}"
    return 1
  fi

  cat "${err}" >&2
  rm -f "${err}"
  return 2
}

# rotate_api_token SECRET_NAME PROJECT SERVER_URL
#   Refresh the stored Woodpecker API token after a JWT-secret rotation (#92),
#   keyed on EXIT STATUS and verified before commit.
#
#   Fixes two bugs at once:
#     1. Secret name: the script read/wrote `woodpecker-api-token`, which does
#        not exist in GCP SM (NOT_FOUND). The real token is `ci-api-token`.
#        Caller now passes the correct name; rotation actually has a token to
#        refresh instead of always falling into "not found, skipping".
#     2. Silent-OK swallow: `gcloud … 2>/dev/null || echo ""` made a real error
#        (PERMISSION_DENIED, transient) indistinguishable from a genuinely-absent
#        secret — both logged "not found, skipping". We now branch on exit
#        status: a real error is surfaced as an error (no misleading "not found").
#
#   Act → Verify (global rule #10): the freshly-minted token is proven to work
#   against the live server BEFORE it overwrites the stored secret, so a bad mint
#   can never replace a working token.
#
#   stdout : status lines (caller forwards to log/alert)
#   return : 0 = rotated and verified
#            1 = secret genuinely absent — nothing to refresh (informational)
#            2 = error (read failed / mint failed / new token failed verification)
#                — caller alerts; the OLD token is left untouched.
rotate_api_token() {
  local secret="$1" project="$2" server="$3"
  local existing rc new_token verify_code

  existing="$(gcloud secrets versions access latest --secret="${secret}" --project="${project}" 2>/dev/null)"
  rc=$?
  if [ "${rc}" -ne 0 ]; then
    # Could not read. Probe existence to tell "absent" from "error".
    if gcloud secrets describe "${secret}" --project="${project}" >/dev/null 2>&1; then
      echo "ERROR: secret ${secret} exists but its latest version is unreadable — API-token rotation skipped (NOT silently OK)"
      return 2
    fi
    echo "INFO: secret ${secret} absent in GCP SM — nothing to rotate (#92)"
    return 1
  fi
  if [ -z "${existing}" ]; then
    echo "ERROR: secret ${secret} read succeeded but is empty — refusing to rotate against an empty token"
    return 2
  fi

  new_token="$(curl -sf --max-time 10 -X POST \
    -H "Authorization: Bearer ${existing}" \
    "${server}/api/user/token" 2>/dev/null)"
  if [ -z "${new_token}" ]; then
    echo "ERROR: could not mint a fresh API token from ${server}/api/user/token — old token left in place (#92)"
    return 2
  fi

  # Verify the NEW token works before committing it (Act → Verify).
  verify_code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
    -H "Authorization: Bearer ${new_token}" \
    "${server}/api/user" 2>/dev/null)"
  if [ "${verify_code}" != "200" ]; then
    echo "ERROR: freshly-minted API token failed verification (GET /api/user => ${verify_code}) — NOT overwriting ${secret} (#92)"
    return 2
  fi

  if ! printf '%s' "${new_token}" | gcloud secrets versions add "${secret}" \
      --data-file=- --project="${project}" >/dev/null 2>&1; then
    echo "ERROR: verified token could not be written to ${secret} — rotation incomplete (#92)"
    return 2
  fi

  echo "OK: API token rotated and verified, new version written to ${secret} (#92)"
  return 0
}

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
