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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateLabelCombination(t *testing.T) {
	cases := []struct {
		name    string
		labels  map[string]string
		wantErr bool
	}{
		// --- legal sets (must NOT error) ---
		{"nil labels", nil, false},
		{"empty labels", map[string]string{}, false},
		{"backend=local alone", map[string]string{LabelFilterBackend: BackendLocal}, false},
		{"tier=spot alone", map[string]string{LabelFilterTier: "spot"}, false},
		{"tier=ondemand alone", map[string]string{LabelFilterTier: "ondemand"}, false},
		{"tier=n2 alone", map[string]string{LabelFilterTier: "n2"}, false},
		{"tier=integration-test alone", map[string]string{LabelFilterTier: "integration-test"}, false},
		{"backend=docker + tier=ondemand (orthogonal, legal)", map[string]string{LabelFilterBackend: "docker", LabelFilterTier: "ondemand"}, false},
		{"backend=local + platform (orthogonal, legal)", map[string]string{LabelFilterBackend: BackendLocal, LabelFilterPlatform: "linux/amd64"}, false},
		{"internal labels ignored", map[string]string{LabelFilterOrg: "123", LabelFilterRepo: "a/b"}, false},
		// empty values are treated as unset by the agent filter, so they must not trip validation
		{"backend=local + empty tier", map[string]string{LabelFilterBackend: BackendLocal, LabelFilterTier: ""}, false},
		{"empty backend + tier=spot", map[string]string{LabelFilterBackend: "", LabelFilterTier: "spot"}, false},
		{"whitespace tier treated as unset", map[string]string{LabelFilterTier: "   "}, false},

		// --- illegal sets (MUST error) — the #255 incident shapes ---
		{"backend=local + tier=ondemand (geometrically impossible)", map[string]string{LabelFilterBackend: BackendLocal, LabelFilterTier: "ondemand"}, true},
		{"backend=local + tier=spot", map[string]string{LabelFilterBackend: BackendLocal, LabelFilterTier: "spot"}, true},
		{"tier=local (undefined third value)", map[string]string{LabelFilterTier: "local"}, true},
		{"unknown tier value", map[string]string{LabelFilterTier: "gigantic"}, true},
		{"backend=local + unknown tier (unknown-tier rule fires first)", map[string]string{LabelFilterBackend: BackendLocal, LabelFilterTier: "local"}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateLabelCombination(tc.labels)
			if tc.wantErr {
				require.Error(t, err)
				assert.ErrorIs(t, err, ErrIncompatibleLabels, "errors must wrap the sentinel for errors.Is callers")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestValidateLabelCombination_MessageNamesTheConflict is the silent-OK
// counterpart: an error is only useful if its message tells the author what to
// fix. Assert the offending labels and the legal sets are quoted.
func TestValidateLabelCombination_MessageNamesTheConflict(t *testing.T) {
	err := ValidateLabelCombination(map[string]string{LabelFilterBackend: BackendLocal, LabelFilterTier: "ondemand"})
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "backend=local")
	assert.Contains(t, msg, "tier=ondemand")
	assert.Contains(t, msg, "spot", "message must list the legal tiers so the author can pick one")
}

// TestKnownTiersAreNonEmpty guards the taxonomy itself: the list must be
// non-empty and every entry must validate as a legal standalone tier (a typo'd
// entry here would otherwise reject the very pipelines it is meant to allow).
func TestKnownTiersAreNonEmpty(t *testing.T) {
	require.NotEmpty(t, KnownTiers)
	for _, tier := range KnownTiers {
		assert.NotEmpty(t, tier)
		assert.NoError(t, ValidateLabelCombination(map[string]string{LabelFilterTier: tier}),
			"every KnownTier must validate as a legal standalone tier")
	}
}

func TestErrIncompatibleLabelsIsSentinel(t *testing.T) {
	assert.True(t, errors.Is(ErrIncompatibleLabels, ErrIncompatibleLabels))
}
