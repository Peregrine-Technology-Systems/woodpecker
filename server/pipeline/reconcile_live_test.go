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

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/queue"
	queue_mocks "go.woodpecker-ci.org/woodpecker/v3/server/queue/mocks"
	"go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
)

// #394 part 1, the mechanism. Pipeline infra#18796 was started at 14:20:42Z
// and killed as orphaned at 14:20:43Z. A pipeline is only marked running by
// rpc.Init, which an agent sends after Poll has already moved the task into
// q.running, so a running pipeline whose task really is in the queue was being
// judged against a queue snapshot taken BEFORE the store read that found it
// running. The snapshot was taken once, at the start of a walk over every repo.
func TestOrphanReconcilerTick_StaleSnapshotDoesNotKillJustStartedPipeline(t *testing.T) {
	mockStore := mocks.NewMockStore(t)
	q := queue_mocks.NewMockQueue(t)
	// First Info: the tick-start snapshot, taken before the pipeline's task was
	// pushed. Every later Info: the task is in q.running.
	q.On("Info", mock.Anything).Return(queue.InfoT{}).Once()
	q.On("Info", mock.Anything).Return(queue.InfoT{
		Running: []*model.Task{{ID: "117374", PipelineID: 100}},
	})

	repos := []*model.Repo{{ID: 9, FullName: "org/infra"}}
	mockStore.On("RepoListAll", true, mock.Anything).Return(repos, nil)
	mockStore.On("GetActivePipelineList", repos[0]).Return([]*model.Pipeline{
		{ID: 100, Number: 18796, Status: model.StatusRunning, RepoID: 9},
	}, nil)

	count := NewOrphanReconciler(q).Tick(mockStore)

	assert.Equal(t, 0, count, "a pipeline whose task is in the queue when it is judged must not be killed")
	mockStore.AssertNotCalled(t, "UpdatePipeline", mock.Anything)
}

// #394 part 1, the backstop ENG specified: a running pipeline with no queue
// task at all, whose running workflow is held by an agent that is still live,
// is not orphaned and must be left alone.
func TestOrphanReconcilerTick_HoldsPipelineWhoseAgentIsLive(t *testing.T) {
	mockStore := mocks.NewMockStore(t)
	q := queueWithTasks(t) // no task for any pipeline, on every read

	repos := []*model.Repo{{ID: 9, FullName: "org/infra"}}
	pl := &model.Pipeline{ID: 100, Number: 18796, Status: model.StatusRunning, RepoID: 9}
	mockStore.On("RepoListAll", true, mock.Anything).Return(repos, nil)
	mockStore.On("GetActivePipelineList", repos[0]).Return([]*model.Pipeline{pl}, nil)
	mockStore.On("WorkflowGetTree", pl).Return([]*model.Workflow{
		{ID: 1, State: model.StatusSuccess, AgentID: 61526},
		{ID: 2, State: model.StatusRunning, AgentID: 61493},
	}, nil)

	r := NewOrphanReconciler(q)
	var asked []int64
	r.SetAgentLiveness(func(agentID int64) bool {
		asked = append(asked, agentID)
		return agentID == 61493
	})
	before := testutil.ToFloat64(reconcileHeldTotal.WithLabelValues(heldAgentLive))

	count := r.Tick(mockStore)

	assert.Equal(t, 0, count, "a pipeline whose running workflow is on a live agent is not orphaned")
	mockStore.AssertNotCalled(t, "UpdatePipeline", mock.Anything)
	assert.Equal(t, []int64{61493}, asked, "only the running workflow's agent is consulted")
	assert.Equal(t, before+1, testutil.ToFloat64(reconcileHeldTotal.WithLabelValues(heldAgentLive)), "a hold is counted, so zero kills can be told apart from reconcile not running")
}

// The reconciler must still kill when the holding agent is gone: otherwise the
// guard disables the backstop it guards.
func TestOrphanReconcilerTick_KillsPipelineWhoseAgentIsGone(t *testing.T) {
	mockStore := mocks.NewMockStore(t)
	q := queueWithTasks(t)

	repos := []*model.Repo{{ID: 9, FullName: "org/infra"}}
	pl := &model.Pipeline{ID: 100, Number: 1, Status: model.StatusRunning, RepoID: 9}
	mockStore.On("RepoListAll", true, mock.Anything).Return(repos, nil)
	mockStore.On("GetActivePipelineList", repos[0]).Return([]*model.Pipeline{pl}, nil)
	mockStore.On("WorkflowGetTree", mock.Anything).Return([]*model.Workflow{
		{ID: 2, State: model.StatusRunning, AgentID: 61493},
	}, nil)
	mockStore.On("UpdatePipeline", mock.Anything).Return(nil)
	mockStore.On("StepListFromWorkflowFind", mock.Anything).Return([]*model.Step{}, nil)
	mockStore.On("WorkflowUpdate", mock.Anything).Return(nil)

	r := NewOrphanReconciler(q)
	r.SetAgentLiveness(func(int64) bool { return false })

	assert.Equal(t, 1, r.Tick(mockStore), "a pipeline whose agent is gone is still killed")
}

// A workflow that was never claimed (AgentID 0) and a pending workflow are not
// evidence of a live holder, even if the oracle would answer true for id 0.
func TestOrphanReconcilerTick_UnclaimedWorkflowIsNotAHolder(t *testing.T) {
	mockStore := mocks.NewMockStore(t)
	q := queueWithTasks(t)

	repos := []*model.Repo{{ID: 9, FullName: "org/infra"}}
	pl := &model.Pipeline{ID: 100, Number: 1, Status: model.StatusRunning, RepoID: 9}
	mockStore.On("RepoListAll", true, mock.Anything).Return(repos, nil)
	mockStore.On("GetActivePipelineList", repos[0]).Return([]*model.Pipeline{pl}, nil)
	mockStore.On("WorkflowGetTree", mock.Anything).Return([]*model.Workflow{
		{ID: 2, State: model.StatusRunning, AgentID: 0},
		{ID: 3, State: model.StatusPending, AgentID: 61493},
	}, nil)
	mockStore.On("UpdatePipeline", mock.Anything).Return(nil)
	mockStore.On("StepListFromWorkflowFind", mock.Anything).Return([]*model.Step{}, nil)
	mockStore.On("WorkflowUpdate", mock.Anything).Return(nil)

	r := NewOrphanReconciler(q)
	r.SetAgentLiveness(func(int64) bool { return true })

	assert.Equal(t, 1, r.Tick(mockStore))
}

// If the workflow tree cannot be read, the reconciler cannot show the pipeline
// is unheld, and a kill is irreversible: hold, and retry on the next tick.
func TestOrphanReconcilerTick_HoldsWhenWorkflowTreeUnreadable(t *testing.T) {
	mockStore := mocks.NewMockStore(t)
	q := queueWithTasks(t)

	repos := []*model.Repo{{ID: 9, FullName: "org/infra"}}
	pl := &model.Pipeline{ID: 100, Number: 1, Status: model.StatusRunning, RepoID: 9}
	mockStore.On("RepoListAll", true, mock.Anything).Return(repos, nil)
	mockStore.On("GetActivePipelineList", repos[0]).Return([]*model.Pipeline{pl}, nil)
	mockStore.On("WorkflowGetTree", pl).Return(nil, assert.AnError)

	r := NewOrphanReconciler(q)
	r.SetAgentLiveness(func(int64) bool { return false })

	assert.Equal(t, 0, r.Tick(mockStore))
	mockStore.AssertNotCalled(t, "UpdatePipeline", mock.Anything)
}
