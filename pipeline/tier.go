// Copyright 2026 Woodpecker Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package pipeline

import (
	"os"
	"strings"
)

// [pts] #266 — submit-time tier auto-routing.
//
// The Peregrine intent has always been: deploy / promote / auto-tag workflows
// run on the `ondemand` tier (a partial deploy, promotion, or release tag is not
// recoverable when a spot VM is preempted mid-flight — the killed-workflow
// re-queue deliberately won't restart a workflow that already did observable
// work). Every other pipeline (ordinary CI, integration tests) stays on spot.
//
// Historically that intent was expressed as a per-repo `tier: ondemand` label in
// each repo's promote/version-bump/deploy YAML. That doesn't scale: a new repo
// has to be told the rule, and a repo that forgets it routes a release pipeline
// to spot and dies on preemption. The blunt reaction — slap `tier: ondemand` on
// everything — over-provisioned on-demand VMs and prompted the guardrail policy
// (scaler#1175) that pushed non-deploy pipelines back to spot.
//
// This classifier is the precise, central mechanism that policy always assumed:
// rewrite `tier` to `ondemand` at submit for exactly the deploy class, so no repo
// needs the knob and none can drift onto spot. See ShouldForceOndemand.

// TierOndemand is the scaling class for non-recoverable work — production
// deploys, promotions, and release tags. It is one of KnownTiers (labels.go).
const TierOndemand = "ondemand"

// EnvDeployPatterns names the env var holding the comma-separated deploy-pattern
// list read by DeployPatterns. Unset (or empty) falls back to
// DefaultDeployPatterns.
const EnvDeployPatterns = "WOODPECKER_TIER_DEPLOY_PATTERNS"

// DefaultDeployPatterns is the built-in deploy-pattern list. A workflow whose
// name contains any of these (case-insensitive substring) is auto-routed to
// ondemand. `sync-back` is deliberately excluded: it is idempotent RELEASE_NOTES
// housekeeping and stays in the spot class the tier guardrail protects.
var DefaultDeployPatterns = []string{"deploy", "promote", "version-bump"}

// DeployPatterns returns the configured deploy-pattern list from
// EnvDeployPatterns (comma-separated; whitespace and empty entries trimmed), or
// DefaultDeployPatterns when the env var is unset or yields no usable entries.
func DeployPatterns() []string {
	raw := strings.TrimSpace(os.Getenv(EnvDeployPatterns))
	if raw == "" {
		return DefaultDeployPatterns
	}

	out := make([]string, 0, strings.Count(raw, ",")+1)
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return DefaultDeployPatterns
	}
	return out
}

// ShouldForceOndemand reports whether a workflow must be pinned to TierOndemand
// at submit time. It is true when the pipeline is tag-triggered (production
// releases are never recoverable on preemption) OR the workflow name contains
// any deploy pattern (case-insensitive substring — the same match shape as the
// scaler's IsDeployPipeline). It is a pure decision; the caller applies the
// rewrite to the workflow's labels.
func ShouldForceOndemand(workflowName string, isTagEvent bool, patterns []string) bool {
	if isTagEvent {
		return true
	}

	name := strings.ToLower(workflowName)
	for _, p := range patterns {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" && strings.Contains(name, p) {
			return true
		}
	}
	return false
}
