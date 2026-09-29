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
  # NOT `stat -f … || stat -c …`: on BSD -f means "format", on GNU it means
  # "filesystem" and EXITS 0, so the fallback never fires and the assertion
  # compares a filesystem dump. Same platform-divergence-that-succeeds shape as
  # the GNU-only sed constructs this estate has been bitten by twice. python3 is
  # already a dependency of the library under test.
  assert_equal "$(python3 -c 'import os,stat,sys; print(oct(stat.S_IMODE(os.stat(sys.argv[1]).st_mode))[2:])' "$out")" "600"
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

# ──────────────── resolve_build_target (#353 inert-merge switch) ────────────────
#
# pts-build.yaml is `event: manual` restricted to `branch: main`, and the wake
# step runs this script from the checked-out workspace — so there is NO way to
# exercise a change from a PR branch, and merging arms the next bake. With the
# create path still unproven, the default must therefore be CURRENT behaviour,
# per the estate's feature-switch rule (a missing switch falls back to the
# committed default set to current behaviour, never to the new one).
#
# One switch selects the whole coherent set rather than three independent vars,
# because independent vars permit incoherent combinations — a PP project with the
# legacy image family, say — that fail in a way that looks like something else.

@test "resolve_build_target: DEFAULT is legacy, so merging is inert" {
  assert_equal "$PTS_BUILD_TARGET_DEFAULT" "legacy"
}

@test "resolve_build_target: legacy -> current behaviour, ambient auth, no mint" {
  resolve_build_target legacy
  assert_equal "$BUILD_PROJECT" "ci-runners-de"
  assert_equal "$BUILD_IMAGE_FAMILY" "ci-agent"
  assert_equal "$BUILD_IMAGE_PROJECT" "ci-runners-de"
  assert_equal "$BUILD_AUTH" "ambient"
}

@test "resolve_build_target: peregrine-production -> PP set with minted auth" {
  resolve_build_target peregrine-production
  assert_equal "$BUILD_PROJECT" "peregrine-production"
  assert_equal "$BUILD_IMAGE_FAMILY" "ci-agent-base-base"
  assert_equal "$BUILD_IMAGE_PROJECT" "peregrine-production"
  assert_equal "$BUILD_AUTH" "pts-build-wake"
}

# A typo must not silently select legacy — that would make an intended cutover
# run silently produce a legacy VM, and the operator would read the green as
# confirmation of the migration.
@test "resolve_build_target: unknown target -> 1 (never a silent default)" {
  run resolve_build_target peregrine-producton
  assert_equal "$status" 1
}

@test "resolve_build_target: empty target -> 1" {
  run resolve_build_target ""
  assert_equal "$status" 1
}

# ═══════════════════ #380: the coherent set gains the bucket ═══════════════════
#
# #601 proved the set was incomplete. Project, image and auth moved to
# peregrine-production; the mint-script bucket did not, so the now-PP-native VM
# identity lost its read on the legacy bucket and the build 403'd — the exact
# mirror of #345/#352, which moved the bucket while the identity stayed legacy.
# The bucket is a member of the set, not a neighbour of it.

@test "resolve_build_target: legacy yields the legacy hooks bucket" {
  resolve_build_target legacy
  assert_equal "$BUILD_HOOKS_BASE" "gs://ci-runners-de-agent-hooks/scripts"
}

@test "resolve_build_target: peregrine-production yields the PP hooks bucket" {
  resolve_build_target peregrine-production
  assert_equal "$BUILD_HOOKS_BASE" "gs://peregrine-production-agent-hooks/scripts"
}

# The whole point of one switch: no combination can be half-migrated. If a future
# edit adds a member and forgets a target, this catches it without anyone having
# to remember the rule.
@test "resolve_build_target: every member of the set is populated, both targets" {
  for t in legacy peregrine-production; do
    unset BUILD_PROJECT BUILD_IMAGE_FAMILY BUILD_IMAGE_PROJECT BUILD_AUTH BUILD_HOOKS_BASE
    resolve_build_target "$t"
    for m in BUILD_PROJECT BUILD_IMAGE_FAMILY BUILD_IMAGE_PROJECT BUILD_AUTH BUILD_HOOKS_BASE; do
      [ -n "${!m}" ] || { echo "target=$t left $m empty"; return 1; }
    done
  done
}

# ══════════════ #380: find_build_vm — absent is NOT the same as errored ═══════
#
# pts-cleanup.sh's list was `... 2>/dev/null | head -1 || true`, and an empty
# result meant "not found — already deleted or never created", exit 0. That
# collapses three states: genuinely absent, gcloud errored, and LOOKING IN THE
# WRONG PROJECT. #601 hit the third — the VM was in peregrine-production, the
# list ran against the legacy project, found nothing, and reported success while
# an e2-standard-8 kept running. Same fail-open shape wake-helpers was hardened
# against for #3089; the cleanup side never got the treatment.

@test "find_build_vm: found -> 0, emits zone and owner" {
  gcloud() { echo "projects/p/zones/us-central1-a  601"; return 0; }
  run find_build_vm proj vm
  assert_success
  assert_output --partial "us-central1-a"
  assert_output --partial "601"
}

@test "find_build_vm: genuinely absent (list ok, no rows) -> 1" {
  gcloud() { echo ""; return 0; }
  run find_build_vm proj vm
  assert_equal "$status" 1
}

@test "find_build_vm: list ERRORED -> 2, never confused with absent" {
  gcloud() { echo "ERROR: (gcloud) API failure" >&2; return 1; }
  run find_build_vm proj vm
  assert_equal "$status" 2
}

# ═══════ #380: delete_build_vm — confirm absence, do not trust the delete ═════
#
# Stubs match the WHOLE arg list, not "$2". `gcloud compute instances delete`
# puts the verb at $3 — matching $2 ("instances") silently never fires the
# delete branch, so three of these tests passed while exercising nothing they
# claimed to. Found by running the real entry point, not by reading them.
#
# The orphan survived a delete that exited 0. The delete's own exit status is
# its report about itself; the only trustworthy check is looking again afterward.

@test "delete_build_vm: gone after delete -> 0" {
  # first call deletes, second call (the confirmation list) shows nothing
  gcloud() { case " $* " in *" delete "*) return 0 ;; esac; echo ""; return 0; }
  run delete_build_vm vm us-central1-a proj
  assert_success
}

@test "delete_build_vm: STILL PRESENT after a delete that exited 0 -> 2" {
  # the #601 shape: delete reports success, the VM is still there
  gcloud() { case " $* " in *" delete "*) return 0 ;; esac; echo "projects/p/zones/us-central1-a  601"; return 0; }
  run delete_build_vm vm us-central1-a proj
  assert_equal "$status" 2
}

@test "delete_build_vm: delete errored and VM still present -> 2" {
  gcloud() { case " $* " in *" delete "*) echo "ERROR: boom" >&2; return 1 ;; esac; echo "projects/p/zones/us-central1-a  601"; return 0; }
  run delete_build_vm vm us-central1-a proj
  assert_equal "$status" 2
}

# A delete that errors because the VM was already gone is a SUCCESS by outcome —
# the post-check is what decides, not the command's complaint.
@test "delete_build_vm: delete errored but VM is gone -> 0 (outcome wins)" {
  gcloud() { case " $* " in *" delete "*) echo "ERROR: was not found" >&2; return 1 ;; esac; echo ""; return 0; }
  run delete_build_vm vm us-central1-a proj
  assert_success
}

@test "delete_build_vm: confirmation list cannot be read -> 2 (undetermined is not success)" {
  gcloud() { case " $* " in *" delete "*) return 0 ;; esac; echo "ERROR: API failure" >&2; return 1; }
  run delete_build_vm vm us-central1-a proj
  assert_equal "$status" 2
}

# ═════════════════ #384: unauthorized list vs empty list ═════════════════
#
# Bake 608 left an e2-standard-8 running while cleanup exited 0 saying
# "genuinely absent". pts-cleanup.sh resolved BUILD_AUTH and never minted, so it
# listed peregrine-production as the ambient legacy identity, and an AGGREGATED
# `gcloud compute instances list` exits 0 with ZERO ROWS for a caller that may
# not look. find_build_vm checked only the exit status, so an unauthorized list
# was indistinguishable from an empty one.
#
# Every pre-existing stub here is AUTHORIZED BY CONSTRUCTION — a shell function
# cannot return "exit 0, no rows, because you may not look here" unless the
# fixture says so. That is why the suite passed while the branch was unreachable
# in production too. These fixtures supply that shape deliberately.

@test "find_build_vm: exit 0 + no rows + permission message on stderr -> 2, NOT absent" {
  gcloud() {
    echo "ERROR: (gcloud.compute.instances.list) Some requests did not succeed:" >&2
    echo " - Required 'compute.instances.list' permission for 'projects/peregrine-production'" >&2
    return 0
  }
  run find_build_vm peregrine-production pts-build-vm
  assert_equal "$status" 2
}

@test "find_build_vm: exit 0 + no rows + clean stderr -> 1 (genuinely absent)" {
  gcloud() { return 0; }
  run find_build_vm proj vm
  assert_equal "$status" 1
}

@test "find_build_vm: rows present wins even if stderr carries a warning" {
  gcloud() {
    echo "WARNING: some zones were unreachable" >&2
    echo "projects/p/zones/us-central1-a  608"
    return 0
  }
  run find_build_vm proj vm
  assert_success
  assert_output --partial "us-central1-a"
}

# ═════════════════ #384: authenticate_for_target ═════════════════

@test "authenticate_for_target: ambient target is a no-op success" {
  resolve_build_target legacy
  run authenticate_for_target /tmp/nonexistent-mint "$BATS_TEST_TMPDIR/tok"
  assert_success
}

@test "authenticate_for_target: pts-build-wake mints and exports the token file" {
  resolve_build_target peregrine-production
  local mint="$BATS_TEST_TMPDIR/mint.sh"
  printf '#!/usr/bin/env bash\nprintf tok > "$1"\n' > "$mint"
  chmod +x "$mint"
  authenticate_for_target "$mint" "$BATS_TEST_TMPDIR/tok"
  assert_equal "$?" 0
  assert_equal "$CLOUDSDK_AUTH_ACCESS_TOKEN_FILE" "$BATS_TEST_TMPDIR/tok"
}

@test "authenticate_for_target: mint failure -> 1 and does NOT export a token" {
  resolve_build_target peregrine-production
  unset CLOUDSDK_AUTH_ACCESS_TOKEN_FILE
  local mint="$BATS_TEST_TMPDIR/bad.sh"
  printf '#!/usr/bin/env bash\nexit 7\n' > "$mint"
  chmod +x "$mint"
  run authenticate_for_target "$mint" "$BATS_TEST_TMPDIR/tok2"
  assert_equal "$status" 1
  assert_equal "${CLOUDSDK_AUTH_ACCESS_TOKEN_FILE:-}" ""
}

@test "authenticate_for_target: mint exits 0 but writes nothing -> 1 (silent-OK)" {
  resolve_build_target peregrine-production
  unset CLOUDSDK_AUTH_ACCESS_TOKEN_FILE
  local mint="$BATS_TEST_TMPDIR/empty.sh"
  printf '#!/usr/bin/env bash\nexit 0\n' > "$mint"
  chmod +x "$mint"
  run authenticate_for_target "$mint" "$BATS_TEST_TMPDIR/tok3"
  assert_equal "$status" 1
}

# ═══════════ #384: pts-cleanup.sh ENTRY POINT — the wiring, not the helper ═══════
#
# The #384 defect was not in a helper. Every helper behaved. The script simply
# never CALLED the auth helper, and no test drove the script, so a suite of
# green helper tests coexisted with a cleanup that looked at the wrong identity
# and reported "genuinely absent" while an e2-standard-8 ran on.
#
# A helper-level test cannot catch "the caller forgot to call me". These drive
# the real entry point as a subprocess, with gcloud shimmed on PATH (a shell
# function is not inherited by a child process), so the assertion is about what
# the SCRIPT does — which is what the caller observes.

@test "pts-cleanup.sh: PP target with an unmintable token -> exit 1 and gcloud is NEVER called" {
  local bin="$BATS_TEST_TMPDIR/bin"
  mkdir -p "$bin"
  printf '#!/usr/bin/env bash\necho "CALLED $*" >> "%s/gcloud-calls"\nexit 0\n' \
    "$BATS_TEST_TMPDIR" > "$bin/gcloud"
  chmod +x "$bin/gcloud"

  PATH="$bin:$PATH" \
  PTS_BUILD_TARGET=peregrine-production \
  PTS_CLEANUP_MINT_SCRIPT="$BATS_TEST_TMPDIR/no-such-mint" \
  run "${BATS_TEST_DIRNAME}/../../scripts/woodpecker/pts-cleanup.sh"

  assert_equal "$status" 1
  assert_output --partial "ORPHANED"
  # The load-bearing assertion: it must not look OR delete as the wrong identity.
  [ ! -f "$BATS_TEST_TMPDIR/gcloud-calls" ]
}

@test "pts-cleanup.sh: legacy target needs no mint and still reaches the lookup" {
  local bin="$BATS_TEST_TMPDIR/bin"
  mkdir -p "$bin"
  # exit 0 with no rows and CLEAN stderr = genuinely absent
  printf '#!/usr/bin/env bash\necho "CALLED $*" >> "%s/gcloud-calls"\nexit 0\n' \
    "$BATS_TEST_TMPDIR" > "$bin/gcloud"
  chmod +x "$bin/gcloud"

  PATH="$bin:$PATH" \
  PTS_BUILD_TARGET=legacy \
  run "${BATS_TEST_DIRNAME}/../../scripts/woodpecker/pts-cleanup.sh"

  assert_success
  assert_output --partial "genuinely absent"
  [ -f "$BATS_TEST_TMPDIR/gcloud-calls" ]
}

@test "pts-cleanup.sh: PP target, mint OK, unauthorized-shaped list -> exit 1, not success" {
  local bin="$BATS_TEST_TMPDIR/bin"
  mkdir -p "$bin"
  # The bake-608 shape: exit 0, zero rows, permission text on stderr.
  cat > "$bin/gcloud" <<'SH'
#!/usr/bin/env bash
echo "ERROR: Required 'compute.instances.list' permission" >&2
exit 0
SH
  chmod +x "$bin/gcloud"
  printf '#!/usr/bin/env bash\nprintf tok > "$1"\n' > "$BATS_TEST_TMPDIR/mint.sh"
  chmod +x "$BATS_TEST_TMPDIR/mint.sh"

  PATH="$bin:$PATH" \
  PTS_BUILD_TARGET=peregrine-production \
  PTS_CLEANUP_MINT_SCRIPT="$BATS_TEST_TMPDIR/mint.sh" \
  run "${BATS_TEST_DIRNAME}/../../scripts/woodpecker/pts-cleanup.sh"

  assert_equal "$status" 1
  assert_output --partial "UNKNOWN"
}
