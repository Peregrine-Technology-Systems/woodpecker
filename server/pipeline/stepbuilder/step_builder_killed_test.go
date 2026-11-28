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

	"go.woodpecker-ci.org/woodpecker/v3/pipeline/frontend/metadata"
	forge_types "go.woodpecker-ci.org/woodpecker/v3/server/forge/types"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
)

// #435 part 2: a workflow's `when: status: killed` reaches the queue task's
// RunOn, so Cancel can see the opt-in. Without it, RunOn stays as before.
func buildRunsOn(t *testing.T, yaml string) []string {
	t.Helper()
	b := StepBuilder{
		Forge:       getMockForge(t),
		RepoTrusted: &metadata.TrustedConfiguration{},
		Repo:        &model.Repo{},
		Curr:        &model.Pipeline{Event: model.EventManual},
		Prev:        &model.Pipeline{},
		Yamls:       []*forge_types.FileMeta{{Data: []byte(yaml)}},
	}
	items, err := b.Build()
	require.NoError(t, err)
	require.Len(t, items, 1)
	return items[0].RunsOn
}

func TestRunsOn_KilledOptIn(t *testing.T) {
	t.Parallel()
	got := buildRunsOn(t, `
when:
  - event: manual
    status: [success, failure, killed]
steps:
  - name: cleanup
    image: scratch
`)
	assert.ElementsMatch(t, []string{"success", "failure", "killed"}, got)
}

func TestRunsOn_NoKilledWithoutOptIn(t *testing.T) {
	t.Parallel()
	got := buildRunsOn(t, `
when:
  - event: manual
    status: [success, failure]
steps:
  - name: notify
    image: scratch
`)
	assert.ElementsMatch(t, []string{"success", "failure"}, got)
}
