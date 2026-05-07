#!/usr/bin/env bash
set -euo pipefail

GO=/usr/local/go/bin/go

echo "==> Running go vet on Peregrine packages..."

# Our packages only — skip web/, cmd/server/, server/router/ (need frontend build)
PACKAGES=(
  go.woodpecker-ci.org/woodpecker/v3/server/plugin/...
  go.woodpecker-ci.org/woodpecker/v3/server/plugin/gcppubsub/...
  go.woodpecker-ci.org/woodpecker/v3/server/plugin/statusapi/...
  go.woodpecker-ci.org/woodpecker/v3/server/plugin/externaldispatch/...
  go.woodpecker-ci.org/woodpecker/v3/server/queue/...
)

"${GO}" vet "${PACKAGES[@]}"

echo "==> Lint passed"
