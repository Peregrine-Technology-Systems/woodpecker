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
