#!/usr/bin/env bash
set -euo pipefail

echo "==> Running tests on Peregrine plugin packages..."

# Our packages only — upstream packages are not our coverage responsibility
PACKAGES=(
  go.woodpecker-ci.org/woodpecker/v3/server/plugin/...
  go.woodpecker-ci.org/woodpecker/v3/server/plugin/gcppubsub/...
  go.woodpecker-ci.org/woodpecker/v3/server/plugin/statusapi/...
  go.woodpecker-ci.org/woodpecker/v3/server/plugin/externaldispatch/...
)

# Skip-CI: if code tree matches main, tests already passed
HEAD_TREE=$(git rev-parse HEAD^{tree} 2>/dev/null || echo "")
MAIN_TREE=$(git rev-parse origin/main^{tree} 2>/dev/null || echo "")
if [ -n "$HEAD_TREE" ] && [ "$HEAD_TREE" = "$MAIN_TREE" ]; then
  echo "==> Skipping: file content identical to main (already tested)"
  exit 0
fi

go test -v -count=1 "${PACKAGES[@]}"

echo "==> Tests passed"
