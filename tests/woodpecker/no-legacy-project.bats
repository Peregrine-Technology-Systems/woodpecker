#!/usr/bin/env bats
# #433: pts-build no longer reaches the legacy project by default. Phase 3c
# woke ci-image-builder in ci-runners-de on every bake, whatever the build
# target: a hardcoded project, and an image family nothing pins any more
# (infra#8873). It is gone.

@test "pts-build.sh no longer wakes ci-image-builder" {
  run grep -nE '^[^#]*(ci-image-builder|image-builder-state|BUILDER_PROJECT)' "${BATS_TEST_DIRNAME}/../../scripts/woodpecker/pts-build.sh"
  assert_failure
}

setup() {
  load 'bats-deps/bats-support/load'
  load 'bats-deps/bats-assert/load'
}
