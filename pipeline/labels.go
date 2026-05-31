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
	"errors"
	"fmt"
	"slices"
	"strings"
)

// [pts] #255 — queue-acceptance label-combination validation.
//
// Woodpecker matches a task's labels against agent labels (server/rpc/filter.go):
// every non-empty task label must be present on an agent. If no agent can ever
// satisfy the combination, the task sits pending forever with no error surface —
// the failure is only noticed downstream (operator, user complaint, scaler
// queue-healthy gauge). This validator moves the failure to submit time so the
// author sees it at `gh pr create`, not in incident archaeology.
//
// The taxonomy below is Peregrine-specific (the `tier` label is a routing
// convention consumed by peregrine-ci-scaler, not upstream Woodpecker). This is
// the single source of truth for legal `backend`/`tier` combinations; keep it in
// sync with docs/ARCHITECTURE.md and the scaler's tier definitions.

// ErrIncompatibleLabels is returned by ValidateLabelCombination when a task's
// labels can never be satisfied by any agent. Callers surface it as a blocking
// pipeline error (status errored).
var ErrIncompatibleLabels = errors.New("incompatible label combination")

// BackendLocal is the backend-label value for the co-located d3ci42-local agent.
// Local-backend tasks run only on that agent, which carries no GCP tier.
const BackendLocal = "local"

// KnownTiers is the set of legal `tier` values — the GCP agent VM classes the
// scaler provisions. A `tier` outside this set (e.g. the historically-undefined
// `tier: local`) is a misconfiguration and rejected at submit. Update this list
// — in lockstep with the scaler — when a new VM class is introduced.
var KnownTiers = []string{"spot", "ondemand", "n2", "integration-test"}

// ValidateLabelCombination rejects task label sets that no agent can satisfy.
//
// Rules (#255):
//  1. If `tier` is set (non-empty), it must be one of KnownTiers.
//  2. `backend=local` may not be combined with any `tier` — the local-backend
//     agent is d3ci42-local, which carries no GCP tier, so the combination is
//     geometrically unsatisfiable.
//
// Empty-valued labels are treated as unset (the agent filter ignores them).
// A nil/empty map is valid. Internal labels (org-id, repo, …) are orthogonal
// and ignored here.
func ValidateLabelCombination(labels map[string]string) error {
	tier := strings.TrimSpace(labels[LabelFilterTier])
	backend := strings.TrimSpace(labels[LabelFilterBackend])

	if tier != "" && !slices.Contains(KnownTiers, tier) {
		return fmt.Errorf("%w: unknown tier=%q (valid tiers: %s)",
			ErrIncompatibleLabels, tier, strings.Join(KnownTiers, ", "))
	}

	if backend == BackendLocal && tier != "" {
		return fmt.Errorf("%w: backend=%s conflicts with tier=%s — local-backend tasks run on the d3ci42-local agent, which carries no GCP tier (legal sets: {backend=local} alone, or {tier in [%s]})",
			ErrIncompatibleLabels, BackendLocal, tier, strings.Join(KnownTiers, ", "))
	}

	return nil
}
