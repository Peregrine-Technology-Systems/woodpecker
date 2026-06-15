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
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	corepipeline "go.woodpecker-ci.org/woodpecker/v3/pipeline"
	"go.woodpecker-ci.org/woodpecker/v3/server"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/pipeline/stepbuilder"
	queue_mocks "go.woodpecker-ci.org/woodpecker/v3/server/queue/mocks"
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

// capturePushedTasks installs a mock queue that records the tasks pushed via
// PushAtOnce, so a test can inspect the routing labels that actually reach the
// queue (the bits the scaler matches agents against).
func capturePushedTasks(t *testing.T) *[]*model.Task {
	t.Helper()
	captured := &[]*model.Task{}
	mockQueue := queue_mocks.NewMockQueue(t)
	mockQueue.On("PushAtOnce", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			*captured = args.Get(1).([]*model.Task)
		}).Return(nil).Once()
	server.Config.Services.Queue = mockQueue
	return captured
}

// TestQueuePipelineForcesDeployTier is the #293 regression guard. The tier
// rewrite must happen at the queuePipeline chokepoint — the single point every
// path (Create, Restart, Approve, future) funnels through via start() — so a
// deploy-class workflow reaching the queue by ANY path carries tier=ondemand,
// not just the Create() path that #266 originally patched. Before the fix,
// Restart()/Approve() enqueued deploy-class tasks with no tier label and spot
// agents claimed them (peregrine-ci-scaler#1616).
func TestQueuePipelineForcesDeployTier(t *testing.T) {
	repo := &model.Repo{ID: 1, FullName: "peregrine/identity-worker", OrgID: 9}

	tierByName := func(tasks []*model.Task) map[string]string {
		out := map[string]string{}
		for _, tk := range tasks {
			out[tk.Name] = tk.Labels[corepipeline.LabelFilterTier]
		}
		return out
	}

	t.Run("push event: deploy-class forced, ci untouched", func(t *testing.T) {
		captured := capturePushedTasks(t)
		// The exact incident workflows (#1616): promote/version-bump enqueued
		// with no tier label on a restart-style path + a normal ci on spot.
		items := []*stepbuilder.Item{
			item("promote", nil),
			item("version-bump", nil),
			item("deploy-production", nil),
			item("ci", map[string]string{corepipeline.LabelFilterTier: "spot"}),
		}
		require.NoError(t, queuePipeline(context.Background(), repo, false, items))

		got := tierByName(*captured)
		assert.Equal(t, corepipeline.TierOndemand, got["promote"], "promote must reach the queue as ondemand")
		assert.Equal(t, corepipeline.TierOndemand, got["version-bump"], "version-bump must reach the queue as ondemand")
		assert.Equal(t, corepipeline.TierOndemand, got["deploy-production"], "deploy must reach the queue as ondemand")
		assert.Equal(t, "spot", got["ci"], "a non-deploy workflow keeps its spot tier")
	})

	t.Run("tag event: image-build forced to ondemand", func(t *testing.T) {
		captured := capturePushedTasks(t)
		items := []*stepbuilder.Item{item("image-build", nil)}
		require.NoError(t, queuePipeline(context.Background(), repo, true, items))

		assert.Equal(t, corepipeline.TierOndemand, tierByName(*captured)["image-build"],
			"a tag-event workflow must reach the queue as ondemand")
	})
}

// TestQueuePipelineTaskBuilding covers queuePipeline's task-construction branches:
// skipped workflows are not queued, dependencies are resolved to task IDs, and a
// nil repo surfaces the label error instead of pushing.
func TestQueuePipelineTaskBuilding(t *testing.T) {
	repo := &model.Repo{ID: 1, FullName: "peregrine/identity-worker", OrgID: 9}

	t.Run("skipped workflows are not queued and dependencies resolve", func(t *testing.T) {
		captured := capturePushedTasks(t)

		clone := &stepbuilder.Item{Workflow: &model.Workflow{ID: 100, Name: "clone"}}
		test := &stepbuilder.Item{Workflow: &model.Workflow{ID: 101, Name: "test"}, DependsOn: []string{"clone"}}
		skipped := &stepbuilder.Item{Workflow: &model.Workflow{ID: 102, Name: "deploy", State: model.StatusSkipped}}

		require.NoError(t, queuePipeline(context.Background(), repo, false, []*stepbuilder.Item{clone, test, skipped}))

		names := map[string]*model.Task{}
		for _, tk := range *captured {
			names[tk.Name] = tk
		}
		assert.NotContains(t, names, "deploy", "a skipped workflow is never enqueued")
		require.Contains(t, names, "test")
		assert.Equal(t, []string{"100"}, names["test"].Dependencies, "DependsOn resolves to the clone task ID")
	})
}
