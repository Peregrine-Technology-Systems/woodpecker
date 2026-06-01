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
		// --- legal: backend + platform are free-form, only `tier` is governed ---
		{"nil labels", nil, false},
		{"empty labels", map[string]string{}, false},
		{"backend=local alone", map[string]string{LabelFilterBackend: "local"}, false},
		{"backend=docker alone", map[string]string{LabelFilterBackend: "docker"}, false},
		{"tier=spot alone", map[string]string{LabelFilterTier: "spot"}, false},
		{"tier=ondemand alone", map[string]string{LabelFilterTier: "ondemand"}, false},
		{"tier=n2 alone", map[string]string{LabelFilterTier: "n2"}, false},
		{"tier=integration-test alone", map[string]string{LabelFilterTier: "integration-test"}, false},
		// the #261 flip: backend=local + tier is the NORMAL case (native step on a fleet VM), not an error
		{"backend=local + tier=spot (the common case)", map[string]string{LabelFilterBackend: "local", LabelFilterTier: "spot"}, false},
		{"backend=local + tier=ondemand", map[string]string{LabelFilterBackend: "local", LabelFilterTier: "ondemand"}, false},
		{"backend=docker + tier=ondemand", map[string]string{LabelFilterBackend: "docker", LabelFilterTier: "ondemand"}, false},
		// host pin via the local-<host> convention — free-form backend value
		{"backend=local-d3ci42 (host pin) alone", map[string]string{LabelFilterBackend: "local-d3ci42"}, false},
		{"backend=local-stripped (flavor)", map[string]string{LabelFilterBackend: "local-stripped"}, false},
		{"backend=local + platform (orthogonal)", map[string]string{LabelFilterBackend: "local", LabelFilterPlatform: "linux/amd64"}, false},
		{"internal labels ignored", map[string]string{LabelFilterOrg: "123", LabelFilterRepo: "a/b"}, false},
		{"empty tier treated as unset", map[string]string{LabelFilterBackend: "local", LabelFilterTier: ""}, false},
		{"whitespace tier treated as unset", map[string]string{LabelFilterTier: "   "}, false},

		// --- illegal: only an unknown/dead `tier` value ---
		{"tier=local (dead — host-identity is not a scaling class)", map[string]string{LabelFilterTier: "local"}, true},
		{"unknown tier value", map[string]string{LabelFilterTier: "gigantic"}, true},
		{"backend=local + tier=local (unknown tier still fires)", map[string]string{LabelFilterBackend: "local", LabelFilterTier: "local"}, true},
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

// TestValidateLabelCombination_MessageGuidesTheFix is the silent-OK counterpart:
// an error is only useful if its message tells the author what to do. The unknown
// -tier message must name the bad value, list the legal tiers, AND point at the
// backend host-pin convention (the #261 trap was that `tier: local` people wanted
// a host, not a tier).
func TestValidateLabelCombination_MessageGuidesTheFix(t *testing.T) {
	err := ValidateLabelCombination(map[string]string{LabelFilterTier: "local"})
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, `tier="local"`)
	assert.Contains(t, msg, "spot", "must list the legal tiers")
	assert.Contains(t, msg, "backend=local-<host>", "must point at the host-pin convention")
}

// TestBackendValuesAreNeverGated locks the #261 contract: backend is free-form —
// engine, flavor, or a local-<host> host pin — and combining it with any legal
// tier is always allowed. A regression here would re-break the ~90% of pipelines
// that legitimately carry backend=local.
func TestBackendValuesAreNeverGated(t *testing.T) {
	for _, backend := range []string{"local", "docker", "local-d3ci42", "local-stripped", "anything"} {
		for _, tier := range []string{"", "spot", "ondemand"} {
			labels := map[string]string{LabelFilterBackend: backend}
			if tier != "" {
				labels[LabelFilterTier] = tier
			}
			assert.NoError(t, ValidateLabelCombination(labels), "backend=%q tier=%q must be legal", backend, tier)
		}
	}
}

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
