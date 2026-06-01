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

// [pts] #255/#261 — queue-acceptance label validation.
//
// Three orthogonal routing dimensions, each with exactly one job:
//
//	platform  — OS / arch family (linux, …).            "Can it run here?"
//	backend   — execution ENVIRONMENT.                  "What/where does it execute?"
//	tier      — Peregrine scaling/scheduling class.     "What VM class do I want?"
//
// `backend` is FREE-FORM and the validator never gates its values. It ranges from
// the engine (`local`, `docker`) through environment flavors (`local-stripped`)
// to a SPECIFIC HOST via the `local-<host>` convention. Because Woodpecker matches
// labels by exact string, `backend: local` (generic native fleet) and
// `backend: local-d3ci42` (the co-located server box) are distinct buckets:
// generic `local` work can't land on the box, and `local-d3ci42` targets only it.
// This is how a workflow pins to a specific host now that the whole fleet honestly
// advertises `backend: local` (post peregrine-infrastructure#1357).
//
// `tier`, by contrast, is a closed set of scaling classes — a value outside it is
// a typo or a dead convention (notably `tier: local`, which conflated host-identity
// with scaling class and is intentionally rejected; the host pin lives on `backend`).
// This is the ONLY dimension the validator governs. A `backend`+`tier` combination
// is always legal: `backend: local + tier: spot` ("native step on a spot VM") is
// the common case, NOT an error.

// ErrIncompatibleLabels is returned by ValidateLabelCombination when a task's
// labels can never be satisfied by any agent. Callers surface it as a blocking
// pipeline error (status errored).
var ErrIncompatibleLabels = errors.New("incompatible label combination")

// KnownTiers is the set of legal `tier` values — the scaling/scheduling classes
// the scaler provisions. A `tier` outside this set (e.g. the dead `tier: local`)
// is a misconfiguration and rejected at submit. Update this list — in lockstep
// with the scaler — when a new scaling class is introduced. NOTE: a specific host
// is NOT a tier; pin to a host via `backend: local-<host>` instead.
var KnownTiers = []string{"spot", "ondemand", "n2", "integration-test"}

// ValidateLabelCombination rejects task label sets no agent can satisfy.
//
// Sole rule (#255/#261): if `tier` is set (non-empty), it must be one of
// KnownTiers. `backend` and `platform` are free-form constraints and are not
// gated — backend in particular is deliberately extensible (engine → flavor →
// `local-<host>` host pin). Empty-valued and internal (org-id, repo, …) labels
// are ignored. A nil/empty map is valid.
func ValidateLabelCombination(labels map[string]string) error {
	tier := strings.TrimSpace(labels[LabelFilterTier])

	if tier != "" && !slices.Contains(KnownTiers, tier) {
		return fmt.Errorf("%w: unknown tier=%q (valid tiers: %s). A specific host is not a tier — pin to it via backend=local-<host>",
			ErrIncompatibleLabels, tier, strings.Join(KnownTiers, ", "))
	}

	return nil
}
