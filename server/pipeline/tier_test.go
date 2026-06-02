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

	corepipeline "go.woodpecker-ci.org/woodpecker/v3/pipeline"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/pipeline/stepbuilder"
)

func tierOf(it *stepbuilder.Item) string {
	return it.Labels[corepipeline.LabelFilterTier]
}

func TestRewritePipelineTier(t *testing.T) {
	t.Run("deploy-class workflows are forced to ondemand", func(t *testing.T) {
		promote := item("promote", map[string]string{corepipeline.LabelFilterTier: "spot"})
		versionBump := item("version-bump", nil)
		deploy := item("deploy-production", map[string]string{})
		ci := item("ci", map[string]string{corepipeline.LabelFilterTier: "spot"})

		rewritePipelineTier([]*stepbuilder.Item{promote, versionBump, deploy, ci}, false)

		assert.Equal(t, corepipeline.TierOndemand, tierOf(promote), "explicit spot must be overridden")
		assert.Equal(t, corepipeline.TierOndemand, tierOf(versionBump), "nil label map must be created and set")
		assert.Equal(t, corepipeline.TierOndemand, tierOf(deploy))
		assert.Equal(t, "spot", tierOf(ci), "non-deploy workflow is left on spot")
	})

	t.Run("tag event forces ondemand for every non-skipped workflow", func(t *testing.T) {
		ci := item("ci", map[string]string{corepipeline.LabelFilterTier: "spot"})
		build := item("build", nil)

		rewritePipelineTier([]*stepbuilder.Item{ci, build}, true)

		assert.Equal(t, corepipeline.TierOndemand, tierOf(ci))
		assert.Equal(t, corepipeline.TierOndemand, tierOf(build))
	})

	t.Run("skipped workflows are left untouched", func(t *testing.T) {
		skipped := item("deploy", map[string]string{corepipeline.LabelFilterTier: "spot"})
		skipped.Workflow.State = model.StatusSkipped

		rewritePipelineTier([]*stepbuilder.Item{skipped}, true)

		assert.Equal(t, "spot", tierOf(skipped), "a skipped workflow never queues, so it is not rewritten")
	})

	t.Run("the forced value passes label validation", func(t *testing.T) {
		// rewrite-then-validate must not produce a pipeline the validator rejects.
		deploy := item("deploy", map[string]string{corepipeline.LabelFilterTier: "local"}) // dead tier
		rewritePipelineTier([]*stepbuilder.Item{deploy}, false)
		assert.NoError(t, validatePipelineLabels([]*stepbuilder.Item{deploy}),
			"forcing ondemand also repairs a deploy workflow that had a dead tier")
	})
}
