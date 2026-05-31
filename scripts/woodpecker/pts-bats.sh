#!/usr/bin/env bash
# pts-bats.sh — run the fork's bats suites against the vendored bats-core.
# bats + bats-support/bats-assert are vendored under tests/woodpecker/bats-deps
# so the suite is self-contained on ephemeral CI VMs (no system bats, no image
# dependency — same "vendor your test deps" rule as the Go integration tests).
# (Not named vendor/ — that path is gitignored for Go module vendoring.)
# python3 is used by one helper (get_pipeline_status) and is present on the
# ci-agent image (pts-wake.sh relies on it too).
set -euo pipefail

BATS="tests/woodpecker/bats-deps/bats-core/bin/bats"

echo "==> Running bats suites (vendored bats $("${BATS}" --version 2>/dev/null || echo '?'))..."
"${BATS}" tests/woodpecker/*.bats
echo "==> bats passed"
