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
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestShouldForceOndemand(t *testing.T) {
	patterns := DefaultDeployPatterns

	cases := []struct {
		name     string
		workflow string
		isTag    bool
		want     bool
	}{
		// tag wins regardless of name
		{"tag event forces ondemand even for a plain CI workflow", "ci", true, true},
		{"tag event forces ondemand for an empty workflow name", "", true, true},

		// deploy-pattern substring matches (case-insensitive)
		{"deploy matches", "deploy", false, true},
		{"deploy as substring matches", "deploy-production", false, true},
		{"promote matches", "promote", false, true},
		{"version-bump matches", "version-bump", false, true},
		{"uppercase still matches", "Deploy-Staging", false, true},
		{"mixed case promote matches", "PromoteToStaging", false, true},

		// the spot class is left alone
		{"plain ci stays spot", "ci", false, false},
		{"integration-test stays spot", "integration-test", false, false},
		{"sync-back stays spot (excluded by default)", "sync-back", false, false},
		{"empty workflow name, non-tag, stays spot", "", false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ShouldForceOndemand(tc.workflow, tc.isTag, patterns))
		})
	}
}

func TestShouldForceOndemandIgnoresBlankPatterns(t *testing.T) {
	// A pattern list of only blanks must never match every workflow.
	assert.False(t, ShouldForceOndemand("ci", false, []string{"", "  "}))
	assert.False(t, ShouldForceOndemand("deploy", false, []string{""}))
	// ...but a tag still forces it.
	assert.True(t, ShouldForceOndemand("ci", true, []string{""}))
}

func TestDeployPatternsDefault(t *testing.T) {
	t.Setenv(EnvDeployPatterns, "")
	assert.Equal(t, DefaultDeployPatterns, DeployPatterns())
}

func TestDeployPatternsFromEnv(t *testing.T) {
	t.Setenv(EnvDeployPatterns, " deploy , release ,, version-bump ")
	assert.Equal(t, []string{"deploy", "release", "version-bump"}, DeployPatterns())
}

func TestDeployPatternsAllBlankFallsBackToDefault(t *testing.T) {
	t.Setenv(EnvDeployPatterns, " , , ")
	assert.Equal(t, DefaultDeployPatterns, DeployPatterns())
}

func TestTierOndemandIsAKnownTier(t *testing.T) {
	// The forced value must pass ValidateLabelCombination — otherwise the
	// rewrite would produce a pipeline the validator rejects.
	assert.Contains(t, KnownTiers, TierOndemand)
	assert.NoError(t, ValidateLabelCombination(map[string]string{LabelFilterTier: TierOndemand}))
}
