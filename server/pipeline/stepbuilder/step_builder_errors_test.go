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

package stepbuilder

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.woodpecker-ci.org/woodpecker/v3/pipeline"
	"go.woodpecker-ci.org/woodpecker/v3/pipeline/frontend/metadata"
	"go.woodpecker-ci.org/woodpecker/v3/server/forge/mocks"
	forge_types "go.woodpecker-ci.org/woodpecker/v3/server/forge/types"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
)

// Coverage for genItemForWorkflow's error paths, which #435 touched.

func lenientForge(t *testing.T) *mocks.MockForge {
	f := mocks.NewMockForge(t)
	f.On("Name").Return("mock").Maybe()
	f.On("URL").Return("https://codeberg.org").Maybe()
	return f
}

func buildOne(t *testing.T, yaml string) ([]*Item, error) {
	t.Helper()
	b := StepBuilder{
		Forge:       lenientForge(t),
		RepoTrusted: &metadata.TrustedConfiguration{},
		Repo:        &model.Repo{},
		Curr:        &model.Pipeline{Event: model.EventManual},
		Prev:        &model.Pipeline{},
		Yamls:       []*forge_types.FileMeta{{Name: "w", Data: []byte(yaml)}},
	}
	return b.Build()
}

func TestGenItem_BadSubstitutionIsAnError(t *testing.T) {
	_, err := buildOne(t, "steps:\n  - name: a\n    image: ${BROKEN\n")
	assert.Error(t, err)
}

func TestGenItem_BadYAMLIsAnError(t *testing.T) {
	_, err := buildOne(t, "steps: [\n")
	assert.Error(t, err)
}

func TestGenItem_BadWhenExpressionIsAnError(t *testing.T) {
	_, err := buildOne(t, "when:\n  - evaluate: '1 +'\nsteps:\n  - name: a\n    image: scratch\n")
	assert.Error(t, err)
}

func TestGenItem_ReservedLabelsAreDropped(t *testing.T) {
	items, err := buildOne(t, "when:\n  - event: manual\nlabels:\n  woodpecker-ci.org/forge-id: spoofed\n  tier: spot\nsteps:\n  - name: a\n    image: scratch\n")
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, "spot", items[0].Labels["tier"])
	assert.NotEqual(t, "spoofed", items[0].Labels[pipeline.LabelForgeRemoteID])
}
