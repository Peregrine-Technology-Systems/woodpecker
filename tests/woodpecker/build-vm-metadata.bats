#!/usr/bin/env bats
# #435: the build VM must never join the spot pool. The agent image's config
# script turns instance metadata `ci-tier` into the agent's `tier` label and
# falls back to "spot" when it is absent; pts-wake set no ci-tier, so the
# build VM advertised tier=spot and took other repos' spot jobs on bakes 654
# and 655. Its metadata now names a tier no fleet workflow routes on.

setup() {
  load 'bats-deps/bats-support/load'
  load 'bats-deps/bats-assert/load'
  source "${BATS_TEST_DIRNAME}/../../scripts/woodpecker/lib/wake-helpers.sh"
}

@test "build_vm_metadata: sets ci-tier=pts-build, so the image cannot default it to spot" {
  run build_vm_metadata 655 us-central1-a
  assert_success
  assert_output --partial "ci-tier=pts-build"
}

@test "build_vm_metadata: never a fleet tier" {
  run build_vm_metadata 655 us-central1-a
  refute_output --partial "ci-tier=spot"
  refute_output --partial "ci-tier=ondemand"
}

@test "build_vm_metadata: keeps the agent label, owner pipeline and zone" {
  run build_vm_metadata 655 us-central1-a
  assert_output "agent-label=pts-build,ci-tier=pts-build,pts-build-pipeline=655,pts-build-zone=us-central1-a"
}

@test "pts-wake.sh creates the VM with build_vm_metadata, not a hand-written list" {
  run grep -E -- '--metadata="\$\(build_vm_metadata ' "${BATS_TEST_DIRNAME}/../../scripts/woodpecker/pts-wake.sh"
  assert_success
  run grep -E -- '--metadata="agent-label=' "${BATS_TEST_DIRNAME}/../../scripts/woodpecker/pts-wake.sh"
  assert_failure
}
