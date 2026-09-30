#!/usr/bin/env bash
# web-build.sh — the cold-cache web UI build for pts-build.sh (#386).
#
# This was inline as `pnpm install ... >/dev/null 2>&1` followed by
# `vite build ... >/dev/null 2>&1`. Both streams discarded, so when the step died
# on bakes 608 and 612 the log ended mid-sentence at "running pnpm build..." and
# the cause could not be read from outside the VM. The cache had never been primed
# on the new bucket, so this fallback ran for the first time in a long while and
# failed invisibly. A failure that cannot name itself costs a bake to diagnose.

# build_web_dist WEB_DIR
#   return : 0 = dist/index.html exists after a build that exited 0
#            1 = any step failed; the failing command's own output was printed
build_web_dist() {
  local dir="$1" log
  if ! command -v pnpm >/dev/null 2>&1; then
    # The agent PROCESS's PATH, not a login shell's: a global npm binary can be
    # present interactively and absent from a service's environment.
    echo "    ❌ pnpm not found on PATH: ${PATH}" >&2
    return 1
  fi
  log="$(mktemp)"
  if ! ( cd "${dir}" && pnpm install --no-frozen-lockfile ) >"${log}" 2>&1; then
    echo "    ❌ pnpm install FAILED — last output:" >&2
    tail -n 40 "${log}" >&2
    rm -f "${log}"
    return 1
  fi
  if ! ( cd "${dir}" && node_modules/.bin/vite build --base=/BASE_PATH ) >"${log}" 2>&1; then
    echo "    ❌ vite build FAILED — last output:" >&2
    tail -n 40 "${log}" >&2
    rm -f "${log}"
    return 1
  fi
  rm -f "${log}"
  # Exit 0 is the tool's report about itself; a build that produced nothing is
  # the silent-OK shape, and the failure would surface later as a missing embed.
  if [ ! -f "${dir}/dist/index.html" ]; then
    echo "    ❌ vite exited 0 but ${dir}/dist/index.html does not exist" >&2
    return 1
  fi
  return 0
}
