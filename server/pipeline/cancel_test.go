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

	"go.woodpecker-ci.org/woodpecker/v3/server"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/pubsub"
	queue_mocks "go.woodpecker-ci.org/woodpecker/v3/server/queue/mocks"
	"go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
)

// =============================================================================
// Unit tests: findIndependentWorkflows
// =============================================================================

func TestFindIndependentWorkflows(t *testing.T) {
	t.Parallel()

	t.Run("identifies workflows with no dependencies", func(t *testing.T) {
		t.Parallel()
		workflows := []*model.Workflow{
			{ID: 100, Name: "ci", DependsOn: nil},
			{ID: 101, Name: "deploy", DependsOn: []string{}},
			{ID: 102, Name: "promote", DependsOn: []string{"ci"}},
		}

		result := findIndependentWorkflows(workflows)
		assert.True(t, result[100], "workflow with nil DependsOn should be independent")
		assert.True(t, result[101], "workflow with empty DependsOn should be independent")
		assert.False(t, result[102], "workflow with deps should not be independent")
	})

	t.Run("returns empty map for no workflows", func(t *testing.T) {
		t.Parallel()
		result := findIndependentWorkflows(nil)
		assert.Empty(t, result)
	})

	t.Run("all workflows independent when none have dependencies", func(t *testing.T) {
		t.Parallel()
		workflows := []*model.Workflow{
			{ID: 1, Name: "ci", DependsOn: nil},
			{ID: 2, Name: "deploy", DependsOn: []string{}},
		}

		result := findIndependentWorkflows(workflows)
		assert.True(t, result[1])
		assert.True(t, result[2])
	})

	// Regression test: the old implementation used TaskList() which only returns
	// tasks still in the queue. A running workflow's task has been dequeued by
	// the agent (Poll deletes from task store), so it was invisible and got killed.
	// Backend pipeline #3336 was killed this way.
	t.Run("running workflow preserved even after task dequeued", func(t *testing.T) {
		t.Parallel()
		workflows := []*model.Workflow{
			{ID: 100, Name: "ci", State: model.StatusSuccess, DependsOn: nil},
			{ID: 101, Name: "deploy", State: model.StatusRunning, DependsOn: []string{}},
			{ID: 102, Name: "promote", State: model.StatusPending, DependsOn: []string{"ci"}},
		}

		result := findIndependentWorkflows(workflows)
		assert.True(t, result[100], "completed CI should be independent")
		assert.True(t, result[101], "running deploy must be independent — this was the bug")
		assert.False(t, result[102], "promote depends on CI")
	})
}

func TestFindIndependentWorkflows_BackwardsCompat(t *testing.T) {
	t.Parallel()

	// Existing pipelines created before the DependsOn migration won't have
	// the field set. All workflows will have nil DependsOn, making them all
	// appear independent. This is the safe default — better to preserve a
	// workflow that should have been killed than to kill one that should be
	// preserved.
	t.Run("pre-migration workflows all appear independent", func(t *testing.T) {
		t.Parallel()
		workflows := []*model.Workflow{
			{ID: 1, Name: "ci", DependsOn: nil},
			{ID: 2, Name: "deploy", DependsOn: nil},
			{ID: 3, Name: "promote", DependsOn: nil},
		}

		result := findIndependentWorkflows(workflows)
		assert.True(t, result[1])
		assert.True(t, result[2])
		assert.True(t, result[3])
	})
}

// =============================================================================
// Integration tests: Cancel with preserveIndependent
// =============================================================================

// setupCancelTest wires up mock store + queue + pubsub for Cancel tests.
func setupCancelTest(t *testing.T) (*mocks.MockStore, *queue_mocks.MockQueue) {
	t.Helper()

	mockStore := mocks.NewMockStore(t)
	mockQueue := queue_mocks.NewMockQueue(t)

	server.Config.Services.Queue = mockQueue
	server.Config.Services.Pubsub = pubsub.New()

	return mockStore, mockQueue
}

// TestCancel_PreserveIndependent_RunningDeploy is the integration test that
// reproduces the exact production bug from backend pipeline #3336.
//
// Scenario: Pipeline has 3 workflows:
//   - ci (depends_on: nil) — already finished
//   - deploy (depends_on: []) — RUNNING on an agent (task dequeued from queue)
//   - promote (depends_on: ["ci"]) — still pending
//
// A new push triggers cancelPreviousPipelines -> Cancel(preserveIndependent=true).
// Expected: deploy stays running, promote gets canceled, pipeline stays running.
func TestCancel_PreserveIndependent_RunningDeploy(t *testing.T) {
	mockStore, mockQueue := setupCancelTest(t)

	pipeline := &model.Pipeline{
		ID:     1,
		Number: 100,
		Status: model.StatusRunning,
	}

	repo := &model.Repo{ID: 1, FullName: "org/backend"}
	user := &model.User{ID: 1}

	// Workflows as they exist in the DB (persisted DependsOn)
	workflows := []*model.Workflow{
		{
			ID:         10,
			PipelineID: 1,
			Name:       "ci",
			State:      model.StatusSuccess,
			DependsOn:  nil,
		},
		{
			ID:         11,
			PipelineID: 1,
			Name:       "deploy",
			State:      model.StatusRunning,
			DependsOn:  []string{},
			Children:   []*model.Step{},
		},
		{
			ID:         12,
			PipelineID: 1,
			Name:       "promote",
			State:      model.StatusPending,
			DependsOn:  []string{"ci"},
			Children: []*model.Step{
				{ID: 120, State: model.StatusPending},
			},
		},
	}

	mockStore.On("WorkflowGetTree", pipeline).Return(workflows, nil)

	// Only promote (ID=12) should be evicted from queue — not deploy (ID=11)
	mockQueue.On("ErrorAtOnce", mock.Anything, []string{"12"}, mock.Anything).Return(nil)

	// promote workflow -> skipped
	mockStore.On("WorkflowUpdate", mock.MatchedBy(func(w *model.Workflow) bool {
		return w.ID == 12 && w.State == model.StatusSkipped
	})).Return(nil)

	// promote's pending step -> canceled
	mockStore.On("StepUpdate", mock.MatchedBy(func(s *model.Step) bool {
		return s.ID == 120 && s.State == model.StatusCanceled
	})).Return(nil)

	// Pipeline should NOT be killed — deploy is still running
	// (no call to UpdatePipeline expected)

	cancelInfo := &model.CancelInfo{SupersededBy: 101}
	err := Cancel(context.Background(), nil, mockStore, repo, user, pipeline, cancelInfo, true)

	assert.NoError(t, err)
	// Pipeline status should NOT have been updated to killed
	mockStore.AssertNotCalled(t, "UpdatePipeline", mock.Anything)
}

// TestCancel_PreserveIndependent_NoRunningIndependent kills pipeline when
// all independent workflows have finished and only dependent ones remain pending.
func TestCancel_PreserveIndependent_NoRunningIndependent(t *testing.T) {
	mockStore, mockQueue := setupCancelTest(t)

	pipeline := &model.Pipeline{
		ID:     2,
		Number: 200,
		Status: model.StatusRunning,
	}
	repo := &model.Repo{ID: 1, FullName: "org/backend"}
	user := &model.User{ID: 1}

	workflows := []*model.Workflow{
		{
			ID:        20,
			Name:      "ci",
			State:     model.StatusSuccess,
			DependsOn: nil,
		},
		{
			ID:        21,
			Name:      "promote",
			State:     model.StatusPending,
			DependsOn: []string{"ci"},
			Children: []*model.Step{
				{ID: 210, State: model.StatusPending},
			},
		},
	}

	mockStore.On("WorkflowGetTree", pipeline).Return(workflows, nil)

	// promote evicted
	mockQueue.On("ErrorAtOnce", mock.Anything, []string{"21"}, mock.Anything).Return(nil)

	// promote -> skipped
	mockStore.On("WorkflowUpdate", mock.MatchedBy(func(w *model.Workflow) bool {
		return w.ID == 21 && w.State == model.StatusSkipped
	})).Return(nil)

	// promote step -> canceled
	mockStore.On("StepUpdate", mock.MatchedBy(func(s *model.Step) bool {
		return s.ID == 210 && s.State == model.StatusCanceled
	})).Return(nil)

	// No independent workflows still running -> pipeline killed.
	// hasPendingOnly=true (promote is only pending), so status=canceled not killed.
	mockStore.On("UpdatePipeline", mock.MatchedBy(func(p *model.Pipeline) bool {
		return p.ID == 2 && p.Status == model.StatusCanceled
	})).Return(nil)

	// Second WorkflowGetTree call after kill (for publishToTopic)
	mockStore.On("WorkflowGetTree", mock.AnythingOfType("*model.Pipeline")).Return(workflows, nil)

	cancelInfo := &model.CancelInfo{SupersededBy: 201}
	err := Cancel(context.Background(), nil, mockStore, repo, user, pipeline, cancelInfo, true)

	assert.NoError(t, err)
	mockStore.AssertCalled(t, "UpdatePipeline", mock.Anything)
}

// TestCancel_NoPreserve kills all workflows regardless of dependencies.
func TestCancel_NoPreserve(t *testing.T) {
	mockStore, mockQueue := setupCancelTest(t)

	pipeline := &model.Pipeline{
		ID:     3,
		Number: 300,
		Status: model.StatusRunning,
	}
	repo := &model.Repo{ID: 1, FullName: "org/backend"}
	user := &model.User{ID: 1}

	workflows := []*model.Workflow{
		{
			ID:        30,
			Name:      "ci",
			State:     model.StatusRunning,
			DependsOn: nil,
			Children:  []*model.Step{},
		},
		{
			ID:        31,
			Name:      "deploy",
			State:     model.StatusRunning,
			DependsOn: []string{},
			Children:  []*model.Step{},
		},
	}

	mockStore.On("WorkflowGetTree", pipeline).Return(workflows, nil)

	// Both running workflows evicted — preserveIndependent=false
	mockQueue.On("ErrorAtOnce", mock.Anything, mock.MatchedBy(func(ids []string) bool {
		return len(ids) == 2
	}), mock.Anything).Return(nil)

	// Pipeline killed (not pending-only since both are running)
	mockStore.On("UpdatePipeline", mock.MatchedBy(func(p *model.Pipeline) bool {
		return p.ID == 3 && p.Status == model.StatusKilled
	})).Return(nil)

	// Second WorkflowGetTree after kill
	mockStore.On("WorkflowGetTree", mock.AnythingOfType("*model.Pipeline")).Return(workflows, nil)

	cancelInfo := &model.CancelInfo{SupersededBy: 301}
	err := Cancel(context.Background(), nil, mockStore, repo, user, pipeline, cancelInfo, false)

	assert.NoError(t, err)
	mockStore.AssertCalled(t, "UpdatePipeline", mock.Anything)
}

// TestCancel_RejectsNonRunningPipeline verifies guard clause.
func TestCancel_RejectsNonRunningPipeline(t *testing.T) {
	pipeline := &model.Pipeline{
		ID:     4,
		Status: model.StatusSuccess,
	}
	repo := &model.Repo{ID: 1}
	user := &model.User{ID: 1}

	err := Cancel(context.Background(), nil, nil, repo, user, pipeline, nil, false)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "Cannot cancel")
}
