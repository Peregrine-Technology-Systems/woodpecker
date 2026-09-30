#!/usr/bin/env bats
# #386: the web-build fallback in pts-build.sh ran
#   pnpm install ... >/dev/null 2>&1 ; vite build ... >/dev/null 2>&1
# so on bakes 608 and 612 the step died 36s in with NO error in the log and the
# cause was unknowable from outside the VM. These pin that a failure names itself.
#
# pnpm/vite are shims on a controlled PATH: a shell function would not be seen by
# the subshell that `cd`s into the web dir, and a real pnpm would make the
# "pnpm is missing" case depend on the machine running the test.

setup() {
  load 'bats-deps/bats-support/load'
  load 'bats-deps/bats-assert/load'
  source "${BATS_TEST_DIRNAME}/../../scripts/woodpecker/lib/web-build.sh"
  WEB="${BATS_TEST_TMPDIR}/web"
  BIN="${BATS_TEST_TMPDIR}/bin"
  mkdir -p "${WEB}/node_modules/.bin" "${WEB}/dist" "${BIN}"
  for t in bash mktemp tail rm cat seq touch; do ln -s "$(command -v "$t")" "${BIN}/$t"; done
}

shim() { printf '#!/usr/bin/env bash\n%s\n' "$2" > "$1"; chmod +x "$1"; }

@test "build_web_dist: pnpm not on PATH -> 1, and names pnpm and the PATH it looked in" {
  PATH="${BIN}" run build_web_dist "${WEB}"
  assert_equal "$status" 1
  assert_output --partial "pnpm"
  assert_output --partial "PATH"
  assert_output --partial "${BIN}"
}

@test "build_web_dist: pnpm install fails -> 1 and its OWN error is shown" {
  shim "${BIN}/pnpm" 'echo "ERR_PNPM_FETCH_404 registry unreachable" >&2; exit 1'
  PATH="${BIN}" run build_web_dist "${WEB}"
  assert_equal "$status" 1
  assert_output --partial "ERR_PNPM_FETCH_404 registry unreachable"
}

@test "build_web_dist: vite build fails -> 1 and its OWN error is shown" {
  shim "${BIN}/pnpm" 'exit 0'
  shim "${WEB}/node_modules/.bin/vite" 'echo "error during build: cannot resolve ./missing" >&2; exit 1'
  PATH="${BIN}" run build_web_dist "${WEB}"
  assert_equal "$status" 1
  assert_output --partial "cannot resolve ./missing"
}

@test "build_web_dist: both exit 0 but no dist/index.html -> 1 (a build that built nothing)" {
  shim "${BIN}/pnpm" 'exit 0'
  shim "${WEB}/node_modules/.bin/vite" 'exit 0'
  PATH="${BIN}" run build_web_dist "${WEB}"
  assert_equal "$status" 1
  assert_output --partial "index.html"
}

@test "build_web_dist: success -> 0" {
  shim "${BIN}/pnpm" 'exit 0'
  shim "${WEB}/node_modules/.bin/vite" 'touch dist/index.html'
  PATH="${BIN}" run build_web_dist "${WEB}"
  assert_equal "$status" 0
}

@test "build_web_dist: a long failing log is trimmed to its tail, not dumped whole" {
  shim "${BIN}/pnpm" 'for i in $(seq 1 500); do echo "noise line $i" >&2; done; echo "THE-REAL-ERROR" >&2; exit 1'
  PATH="${BIN}" run build_web_dist "${WEB}"
  assert_equal "$status" 1
  assert_output --partial "THE-REAL-ERROR"
  refute_output --partial "noise line 1"
}

# ── wiring: the helper tests above cannot catch "the caller forgot to call me" ──
# (#384's lesson: every helper behaved and the script never invoked one.) pts-build.sh
# is a 300-line procedural script that cannot be driven end to end here, so this pins
# the two facts that make the #386 bug possible, statically.

@test "pts-build.sh calls build_web_dist for the cold-cache web build" {
  run grep -E '^\s*if ! build_web_dist web' "${BATS_TEST_DIRNAME}/../../scripts/woodpecker/pts-build.sh"
  assert_success
}

@test "pts-build.sh no longer discards pnpm/vite output" {
  run grep -E '^[^#]*(pnpm|vite)[^#]*>/dev/null[[:space:]]+2>&1' "${BATS_TEST_DIRNAME}/../../scripts/woodpecker/pts-build.sh"
  assert_equal "$status" 1
}
