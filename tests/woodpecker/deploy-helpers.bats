#!/usr/bin/env bats
# Tests for scripts/woodpecker/lib/deploy-helpers.sh — the #3089 silent-OK
# hardening. Every function is exercised on all three branches it must keep
# distinct: present/happy, genuinely-absent (quiet), and backend-error (loud,
# never silently treated as absent).
#
# Stubs are shell functions (inherited by the command-substitution subshells the
# lib uses), so no system gcloud/gsutil/curl is touched.

setup() {
  load 'bats-deps/bats-support/load'
  load 'bats-deps/bats-assert/load'
  source "${BATS_TEST_DIRNAME}/../../scripts/woodpecker/lib/deploy-helpers.sh"
}

# ─────────────────────────── read_pending_marker ───────────────────────────

@test "read_pending_marker: present -> 0 and emits content" {
  gsutil() { printf 'v3.13.0-pts.440\nabc1234\n440\n'; return 0; }
  run read_pending_marker gs://bucket
  assert_success
  assert_line --index 0 'v3.13.0-pts.440'
}

@test "read_pending_marker: genuinely absent -> 1 and stays quiet" {
  gsutil() { echo 'CommandException: No URLs matched: gs://bucket/pending' >&2; return 1; }
  run read_pending_marker gs://bucket
  assert_equal "$status" 1
  refute_output   # absent must not be noisy
}

@test "read_pending_marker: backend error -> 2 and surfaces the error (NOT silent-OK)" {
  gsutil() { echo 'ServiceException: 401 Anonymous caller does not have storage.objects.get' >&2; return 1; }
  run read_pending_marker gs://bucket
  assert_equal "$status" 2
  assert_output --partial 'ServiceException: 401'
}

# ───────────────────────────── rotate_api_token ─────────────────────────────

# Default stubs for the happy path; individual tests override the one piece they
# want to fail.
_stub_happy() {
  gcloud() {
    case "$1 $2" in
      "secrets versions") [ "$3" = "access" ] && { echo "old-token"; return 0; }
                          [ "$3" = "add" ] && return 0 ;;
      "secrets describe") return 0 ;;
    esac
    return 0
  }
  curl() {
    # last arg is the URL
    local url="${!#}"
    case "$url" in
      */api/user/token) echo "new-token-xyz"; return 0 ;;      # mint
      */api/user)       echo "200"; return 0 ;;                # verify (-w %{http_code})
    esac
    return 0
  }
}

@test "rotate_api_token: happy path -> 0, writes new version" {
  _stub_happy
  run rotate_api_token ci-api-token proj http://localhost:8000
  assert_success
  assert_output --partial 'rotated and verified'
}

@test "rotate_api_token: secret genuinely absent -> 1 (informational, not error)" {
  gcloud() {
    [ "$1 $2 $3" = "secrets versions access" ] && return 1   # cannot read
    [ "$1 $2" = "secrets describe" ] && return 1             # ...and it does not exist
    return 0
  }
  run rotate_api_token ci-api-token proj http://localhost:8000
  assert_equal "$status" 1
  assert_output --partial 'absent'
}

@test "rotate_api_token: exists but latest version unreadable -> 2 (error, not 'not found')" {
  gcloud() {
    [ "$1 $2 $3" = "secrets versions access" ] && return 1   # cannot read latest
    [ "$1 $2" = "secrets describe" ] && return 0             # but the secret EXISTS
    return 0
  }
  run rotate_api_token ci-api-token proj http://localhost:8000
  assert_equal "$status" 2
  assert_output --partial 'unreadable'
}

@test "rotate_api_token: existing token reads empty -> 2 (refuse to rotate on empty)" {
  gcloud() { [ "$1 $2 $3" = "secrets versions access" ] && { echo ""; return 0; }; return 0; }
  run rotate_api_token ci-api-token proj http://localhost:8000
  assert_equal "$status" 2
  assert_output --partial 'empty'
}

@test "rotate_api_token: mint fails (empty new token) -> 2, old token untouched" {
  _stub_happy
  curl() { local url="${!#}"; [ "${url##*/}" = "token" ] && { echo ""; return 1; }; echo "200"; }
  run rotate_api_token ci-api-token proj http://localhost:8000
  assert_equal "$status" 2
  assert_output --partial 'could not mint'
}

@test "rotate_api_token: new token fails verification (non-200) -> 2, NOT written" {
  _stub_happy
  curl() {
    local url="${!#}"
    case "$url" in
      */api/user/token) echo "new-token-xyz"; return 0 ;;
      */api/user)       echo "403"; return 0 ;;   # verification fails
    esac
  }
  run rotate_api_token ci-api-token proj http://localhost:8000
  assert_equal "$status" 2
  assert_output --partial 'failed verification'
}

@test "rotate_api_token: verified token but write fails -> 2" {
  _stub_happy
  gcloud() {
    [ "$1 $2 $3" = "secrets versions access" ] && { echo "old-token"; return 0; }
    [ "$1 $2 $3" = "secrets versions add" ] && return 1   # write fails
    [ "$1 $2" = "secrets describe" ] && return 0
    return 0
  }
  run rotate_api_token ci-api-token proj http://localhost:8000
  assert_equal "$status" 2
  assert_output --partial 'could not be written'
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
