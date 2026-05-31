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
	"github.com/stretchr/testify/require"

	corepipeline "go.woodpecker-ci.org/woodpecker/v3/pipeline"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/pipeline/stepbuilder"
)

func item(name string, labels map[string]string) *stepbuilder.Item {
	return &stepbuilder.Item{
		Workflow: &model.Workflow{Name: name},
		Labels:   labels,
	}
}

func TestValidatePipelineLabels(t *testing.T) {
	t.Run("all legal passes", func(t *testing.T) {
		items := []*stepbuilder.Item{
			item("build", map[string]string{corepipeline.LabelFilterTier: "spot"}),
			item("deploy", map[string]string{corepipeline.LabelFilterTier: "ondemand"}),
			item("local", map[string]string{corepipeline.LabelFilterBackend: corepipeline.BackendLocal}),
		}
		assert.NoError(t, validatePipelineLabels(items))
	})

	t.Run("one unsatisfiable workflow fails the pipeline and is named", func(t *testing.T) {
		items := []*stepbuilder.Item{
			item("build", map[string]string{corepipeline.LabelFilterTier: "spot"}),
			item("deploy-local", map[string]string{corepipeline.LabelFilterBackend: corepipeline.BackendLocal, corepipeline.LabelFilterTier: "ondemand"}),
		}
		err := validatePipelineLabels(items)
		require.Error(t, err)
		assert.ErrorIs(t, err, corepipeline.ErrIncompatibleLabels)
		assert.Contains(t, err.Error(), "deploy-local", "the offending workflow must be named")
	})

	t.Run("skipped workflows are not validated", func(t *testing.T) {
		// A skipped workflow never queues, so its labels can't strand anything —
		// validating it would error pipelines that would otherwise run fine.
		skipped := item("skip-me", map[string]string{corepipeline.LabelFilterTier: "bogus"})
		skipped.Workflow.State = model.StatusSkipped
		items := []*stepbuilder.Item{
			item("build", map[string]string{corepipeline.LabelFilterTier: "spot"}),
			skipped,
		}
		assert.NoError(t, validatePipelineLabels(items))
	})

	t.Run("empty item list passes", func(t *testing.T) {
		assert.NoError(t, validatePipelineLabels(nil))
	})
}
