#!/usr/bin/env bash
# auth-helpers.sh — minting and gcloud authentication for a resolved build target.
#
# Split out of wake-helpers.sh for #384. Authentication became a SHARED concern
# the moment pts-cleanup.sh needed it too, and the bug that issue records is
# exactly what happens when one caller has the behaviour and another only has the
# variable: cleanup resolved BUILD_AUTH, never minted, listed peregrine-production
# as the ambient legacy identity, and reported an unauthorized empty list as
# "genuinely absent" while an e2-standard-8 ran on.
#
# Sourced by wake-helpers.sh, so every existing consumer keeps working unchanged.

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

# authenticate_for_target MINT_SCRIPT TOKEN_FILE
#   Authenticates gcloud for the target resolved by resolve_build_target.
#   return : 0 = authenticated, or the target legitimately uses ambient creds
#            1 = this target REQUIRES a minted identity and it could not be had
#
#   #384: pts-cleanup.sh resolved BUILD_AUTH and never acted on it, so every
#   gcloud call fell back to the ambient legacy identity, which can see nothing
#   in peregrine-production. Resolving an auth member and ignoring it is worse
#   than having no concept of auth, because the resolution reads as though it
#   took effect. Extracted here so wake and cleanup cannot drift apart again:
#   one pattern applied twice, rather than a second copy.
#
#   Exports CLOUDSDK_AUTH_ACCESS_TOKEN_FILE rather than returning a flag for the
#   caller to pass per-call. A missed call site with a flag falls back SILENTLY
#   to the ambient identity; with the env var there is no per-call site to miss.
authenticate_for_target() {
  local mint="$1" out="$2"

  if [ "${BUILD_AUTH:-ambient}" = "ambient" ]; then
    return 0
  fi

  if ! mint_wake_token "${mint}" "${out}"; then
    return 1
  fi

  export CLOUDSDK_AUTH_ACCESS_TOKEN_FILE="${out}"
  return 0
}
