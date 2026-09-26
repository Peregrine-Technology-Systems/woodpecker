#!/usr/bin/env bats
# Tests for scripts/woodpecker/lib/wake-helpers.sh — the #3089 silent-OK
# hardening of the pts-wake.sh #120 mutex. Both functions are exercised on the
# branches they must keep distinct: read-ok (value, possibly empty) vs
# could-not-determine (error), so an undetermined owner/status never falls
# through to the destructive `gcloud … instances delete`.
#
# Stubs are shell functions (inherited by the command-substitution subshells the
# lib uses), so no system gcloud/curl is touched.

setup() {
  load 'bats-deps/bats-support/load'
  load 'bats-deps/bats-assert/load'
  source "${BATS_TEST_DIRNAME}/../../scripts/woodpecker/lib/wake-helpers.sh"
}

# ──────────────────────── get_vm_owner_pipeline ────────────────────────

@test "get_vm_owner_pipeline: read ok with owner -> 0 and emits id" {
  gcloud() { echo "439"; return 0; }
  run get_vm_owner_pipeline vm zone proj
  assert_success
  assert_output "439"
}

@test "get_vm_owner_pipeline: read ok, genuinely no owner -> 0 and empty" {
  gcloud() { echo ""; return 0; }
  run get_vm_owner_pipeline vm zone proj
  assert_success
  refute_output
}

@test "get_vm_owner_pipeline: read error -> 2 (caller must NOT treat as unowned)" {
  gcloud() { echo "ERROR: (gcloud) some API failure" >&2; return 1; }
  run get_vm_owner_pipeline vm zone proj
  assert_equal "$status" 2
}

# ───────────────────────── get_pipeline_status ─────────────────────────

@test "get_pipeline_status: ok -> 0 and emits status" {
  curl() { echo '{"status":"running","id":1}'; return 0; }
  run get_pipeline_status http://wp tok 439
  assert_success
  assert_output "running"
}

@test "get_pipeline_status: HTTP error -> 2" {
  curl() { return 22; }   # curl -sf on 4xx/5xx
  run get_pipeline_status http://wp tok 439
  assert_equal "$status" 2
}

@test "get_pipeline_status: unparseable body -> 2 (not a silent 'unknown')" {
  curl() { echo '<html>502 Bad Gateway</html>'; return 0; }
  run get_pipeline_status http://wp tok 439
  assert_equal "$status" 2
}

# ───────────────────────── mint_wake_token (#353) ─────────────────────────
#
# pts-wake.sh used to authenticate not at all, inheriting whatever identity was
# ambient on the host — the legacy one, which cannot be granted anything on
# peregrine-production and so 403'd in all 11 zones (#354/#355). It now mints a
# token for the dedicated wake identity. The load-bearing property is that a
# failure to obtain a usable token must ABORT, never fall through to the ambient
# identity: a silent fallback reproduces the exact 403 this fixes and presents as
# a permissions problem rather than an authentication one.

@test "mint_wake_token: mint script absent -> 2 (caller must abort)" {
  run mint_wake_token "${BATS_TEST_TMPDIR}/no-such-mint.sh" "${BATS_TEST_TMPDIR}/tok"
  assert_equal "$status" 2
}

@test "mint_wake_token: mint script present but not executable -> 2" {
  local m="${BATS_TEST_TMPDIR}/mint.sh"
  printf '#!/bin/sh\necho tok > "$1"\n' > "$m"
  chmod -x "$m"
  run mint_wake_token "$m" "${BATS_TEST_TMPDIR}/tok"
  assert_equal "$status" 2
}

@test "mint_wake_token: mint exits non-zero -> 2" {
  local m="${BATS_TEST_TMPDIR}/mint.sh"
  printf '#!/bin/sh\necho "boom" >&2\nexit 1\n' > "$m"; chmod +x "$m"
  run mint_wake_token "$m" "${BATS_TEST_TMPDIR}/tok"
  assert_equal "$status" 2
}

# The silent-OK counterpart: infra's own bakery runbook warns "NEVER 2>/dev/null
# a mint; a silent mint failure looks like a permission problem three commands
# later". An exit-0 mint that writes nothing is exactly that shape, so the
# emptiness is checked rather than the exit status alone.
@test "mint_wake_token: mint exits 0 but writes an EMPTY token -> 2" {
  local m="${BATS_TEST_TMPDIR}/mint.sh"
  printf '#!/bin/sh\n: > "$1"\nexit 0\n' > "$m"; chmod +x "$m"
  run mint_wake_token "$m" "${BATS_TEST_TMPDIR}/tok"
  assert_equal "$status" 2
}

@test "mint_wake_token: mint exits 0 but writes only whitespace -> 2" {
  local m="${BATS_TEST_TMPDIR}/mint.sh"
  printf '#!/bin/sh\nprintf "\\n  \\n" > "$1"\nexit 0\n' > "$m"; chmod +x "$m"
  run mint_wake_token "$m" "${BATS_TEST_TMPDIR}/tok"
  assert_equal "$status" 2
}

@test "mint_wake_token: mint writes a real token -> 0 and file is usable" {
  local m="${BATS_TEST_TMPDIR}/mint.sh" out="${BATS_TEST_TMPDIR}/tok"
  printf '#!/bin/sh\nprintf "ya29.fake-token" > "$1"\nexit 0\n' > "$m"; chmod +x "$m"
  run mint_wake_token "$m" "$out"
  assert_success
  [ -s "$out" ]
  assert_equal "$(cat "$out")" "ya29.fake-token"
}

@test "mint_wake_token: token file is created mode 600" {
  local m="${BATS_TEST_TMPDIR}/mint.sh" out="${BATS_TEST_TMPDIR}/tok"
  printf '#!/bin/sh\nprintf "ya29.fake-token" > "$1"\nexit 0\n' > "$m"; chmod +x "$m"
  run mint_wake_token "$m" "$out"
  assert_success
  assert_equal "$(stat -f '%OLp' "$out" 2>/dev/null || stat -c '%a' "$out")" "600"
}

# ──────────────── zone_failure_is_retryable (the #369 residue) ────────────────
#
# The zone-fallback loop printed "capacity unavailable" for EVERY `gcloud create`
# failure and then "all zones at capacity" after eleven of them. That is a cause
# the script never observed: pipeline #581's permanent 403 was retried in all 11
# zones and summarised as a stockout, and the auth failure introduced by this very
# change was reported the same way. Only a genuine per-zone stockout is worth
# another zone; anything else must fail immediately with its real reason.

@test "zone_failure_is_retryable: ZONE_RESOURCE_POOL_EXHAUSTED -> retryable" {
  run zone_failure_is_retryable "ERROR: ZONE_RESOURCE_POOL_EXHAUSTED: The zone does not have enough resources"
  assert_success
}

@test "zone_failure_is_retryable: 'does not have enough resources' -> retryable" {
  run zone_failure_is_retryable "The zone 'us-east1-b' does not have enough resources available"
  assert_success
}

@test "zone_failure_is_retryable: invalid auth credentials -> NOT retryable" {
  run zone_failure_is_retryable "Request had invalid authentication credentials. Expected OAuth 2 access token"
  # exact status, not assert_failure: a MISSING function exits 127, which is
  # also "failure", so assert_failure would pass on an absent implementation.
  assert_equal "$status" 1
}

@test "zone_failure_is_retryable: missing permission (the #581 403) -> NOT retryable" {
  run zone_failure_is_retryable "Required 'compute.instances.create' permission for 'projects/x/zones/y/instances/z'"
  # exact status, not assert_failure: a MISSING function exits 127, which is
  # also "failure", so assert_failure would pass on an absent implementation.
  assert_equal "$status" 1
}

# Quota is global, not per-zone: switching zones provably does not help, and a
# previous bake wedge was diagnosed only after 11 pointless attempts.
@test "zone_failure_is_retryable: CPUS_ALL_REGIONS quota -> NOT retryable" {
  run zone_failure_is_retryable "Quota 'CPUS_ALL_REGIONS' exceeded. Limit: 64.0 globally."
  # exact status, not assert_failure: a MISSING function exits 127, which is
  # also "failure", so assert_failure would pass on an absent implementation.
  assert_equal "$status" 1
}

@test "zone_failure_is_retryable: image not found -> NOT retryable" {
  run zone_failure_is_retryable "The resource 'projects/p/global/images/family/ci-agent-base-base' was not found"
  # exact status, not assert_failure: a MISSING function exits 127, which is
  # also "failure", so assert_failure would pass on an absent implementation.
  assert_equal "$status" 1
}

# Fail-safe default: an unrecognised error is reported, not retried eleven times
# and then relabelled as capacity.
@test "zone_failure_is_retryable: unrecognised error -> NOT retryable (report it)" {
  run zone_failure_is_retryable "ERROR: something nobody has seen before"
  # exact status, not assert_failure: a MISSING function exits 127, which is
  # also "failure", so assert_failure would pass on an absent implementation.
  assert_equal "$status" 1
}
