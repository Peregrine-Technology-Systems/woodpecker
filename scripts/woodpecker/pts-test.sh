#!/usr/bin/env bash
set -euo pipefail

# Go toolchain resolution (#369). The CI fleet moved from the packer image,
# which untarred Go to /usr/local/go/bin, to the bakery image (since
# ~2026-08-26), which installs it under /opt/go/<version>/go/bin and links
# /usr/local/bin/go — already on PATH. The old absolute path no longer exists
# by design (infra#6594), so resolve through PATH and fail loud if it is
# genuinely missing rather than dying later with a bare "command not found".
# GO stays overridable for a local run against a non-default toolchain.
GO="${GO:-$(command -v go || true)}"
if [ -z "${GO}" ] || [ ! -x "${GO}" ]; then
    echo "ERROR: no usable go found on PATH (and \$GO unset/invalid)." >&2
    echo "       Bakery image links /usr/local/bin/go; packer image used /usr/local/go/bin/go." >&2
    echo "       Set GO=/path/to/go to override. Not guessing a path (#369)." >&2
    exit 1
fi

echo "==> Running tests on Peregrine plugin packages..."

# Our packages only — upstream packages are not our coverage responsibility.
# CPU-timing-tolerant packages — safe to run in one concurrent invocation.
PACKAGES=(
  go.woodpecker-ci.org/woodpecker/v3/server/plugin/...
  go.woodpecker-ci.org/woodpecker/v3/server/plugin/gcppubsub/...
  go.woodpecker-ci.org/woodpecker/v3/server/plugin/statusapi/...
  go.woodpecker-ci.org/woodpecker/v3/server/plugin/externaldispatch/...
  go.woodpecker-ci.org/woodpecker/v3/server/rpc
  go.woodpecker-ci.org/woodpecker/v3/server/forge/github
)

# agent/rpc drives REAL WebSocket round-trips with real timeouts. Bundled into
# the invocation above, `go test` runs its binary concurrently with the other
# package binaries (up to GOMAXPROCS), and on a constrained CI spot agent that
# cross-package competition starves its readPump goroutine so a reply isn't
# routed within the assertion's timeout — flaking the timing tests (#325). Give
# it a dedicated invocation so it runs against the agent's full CPU, with no
# sibling test binaries competing for scheduling.
TIMING_PACKAGES=(
  go.woodpecker-ci.org/woodpecker/v3/agent/rpc
)

# Skip-CI: if code tree matches main, tests already passed
HEAD_TREE=$(git rev-parse HEAD^{tree} 2>/dev/null || echo "")
MAIN_TREE=$(git rev-parse origin/main^{tree} 2>/dev/null || echo "")
if [ -n "$HEAD_TREE" ] && [ "$HEAD_TREE" = "$MAIN_TREE" ]; then
  echo "==> Skipping: file content identical to main (already tested)"
  exit 0
fi

"${GO}" test -v -count=1 "${PACKAGES[@]}"

echo "==> Running timing-sensitive packages in isolation..."
"${GO}" test -v -count=1 "${TIMING_PACKAGES[@]}"

echo "==> Tests passed"
