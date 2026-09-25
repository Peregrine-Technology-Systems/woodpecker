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
