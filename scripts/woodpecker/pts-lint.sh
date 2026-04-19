#!/usr/bin/env bash
set -euo pipefail

# Packer puts Go at /usr/local/go/bin; /etc/profile.d/go.sh adds it to PATH for
# login shells only. This script runs under non-login bash, so export directly.
export PATH="/usr/local/go/bin:$PATH"

echo "==> Running go vet on Peregrine packages..."

# Our packages only — skip web/, cmd/server/, server/router/ (need frontend build)
PACKAGES=(
  go.woodpecker-ci.org/woodpecker/v3/server/plugin/...
  go.woodpecker-ci.org/woodpecker/v3/server/plugin/gcppubsub/...
  go.woodpecker-ci.org/woodpecker/v3/server/plugin/statusapi/...
  go.woodpecker-ci.org/woodpecker/v3/server/plugin/externaldispatch/...
  go.woodpecker-ci.org/woodpecker/v3/server/queue/...
)

go vet "${PACKAGES[@]}"

echo "==> Lint passed"
