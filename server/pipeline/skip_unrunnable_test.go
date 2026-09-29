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

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/queue"
	queue_mocks "go.woodpecker-ci.org/woodpecker/v3/server/queue/mocks"
	store_mocks "go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
)

// newWedgeQueue builds a REAL fifo queue holding a wake→compile→cleanup chain,
// then fails wake — reproducing #382's shape.
//
// NO WORKER IS EVER REGISTERED. That is the whole point: the bug is that the
// skip decision lives in the agent poll path, so any test that registers an
// agent able to claim the dependent exercises the already-working path and
// passes while the defect stands. Criterion 5 of #382.
func newWedgeQueue(ctx context.Context, t *testing.T) queue.Queue {
	t.Helper()
	q := queue.NewMemoryQueue(ctx)
	tasks := []*model.Task{
		{ID: "1", Labels: map[string]string{"backend": "local-d3ci42"}},
		// pinned to an agent that wake was supposed to create, and never did
		{ID: "2", Labels: map[string]string{"agent": "pts-build"}, Dependencies: []string{"1"}, DepStatus: map[string]model.StatusValue{}},
		{ID: "3", Labels: map[string]string{"backend": "local-d3ci42"}, Dependencies: []string{"2"}, DepStatus: map[string]model.StatusValue{}},
	}
	assert.NoError(t, q.PushAtOnce(ctx, tasks))
	// wake fails, exactly as bakes 603 and 606 did
	assert.NoError(t, q.Done(ctx, "1", model.StatusFailure))
	return q
}

func wedgePipeline() *model.Pipeline {
	return &model.Pipeline{
		ID: 7,
		Workflows: []*model.Workflow{
			{ID: 1, Name: "pts-build", State: model.StatusFailure},
			{ID: 2, Name: "pts-build-compile", State: model.StatusPending, DependsOn: []string{"pts-build"}},
			{ID: 3, Name: "pts-build-cleanup", State: model.StatusPending, DependsOn: []string{"pts-build-compile"}},
		},
	}
}

// Criteria 1 and 2: the pipeline settles with no agent ever polling, and the
// transitive dependent settles too.
func TestSkipUnrunnableWorkflowsSettlesChainWithNoAgents(t *testing.T) {
	ctx := context.Background()
	q := newWedgeQueue(ctx, t)
	pl := wedgePipeline()

	mockStore := store_mocks.NewMockStore(t)
	mockStore.On("WorkflowUpdate", mock.Anything).Return(nil)

	skipped := SkipUnrunnableWorkflows(ctx, mockStore, q, pl)

	assert.Equal(t, 2, skipped, "both the pinned dependent and its own dependent should settle")
	assert.Equal(t, model.StatusSkipped, pl.Workflows[1].State, "pts-build-compile")
	assert.Equal(t, model.StatusSkipped, pl.Workflows[2].State, "pts-build-cleanup")
	assert.NotZero(t, pl.Workflows[1].Finished, "a settled workflow must carry a finish time")

	// Criterion 4: nothing of this pipeline is left occupying the queue.
	info := q.Info(ctx)
	assert.Empty(t, info.Pending, "no pending tasks should remain")
	assert.Empty(t, info.WaitingOnDeps, "no waiting tasks should remain")

	// The pipeline can now finalize, which is the user-visible symptom.
	assert.False(t, model.IsThereRunningStage(pl.Workflows),
		"pipeline must no longer look like it is still running")
}

// Criterion 3: run_on failure must not regress — a dependent that declares it
// runs on failure is still eligible and must NOT be skipped.
func TestSkipUnrunnableWorkflowsKeepsRunOnFailureDependent(t *testing.T) {
	ctx := context.Background()
	q := queue.NewMemoryQueue(ctx)
	tasks := []*model.Task{
		{ID: "1"},
		{ID: "2", Dependencies: []string{"1"}, DepStatus: map[string]model.StatusValue{}, RunOn: []string{"success", "failure"}},
	}
	assert.NoError(t, q.PushAtOnce(ctx, tasks))
	assert.NoError(t, q.Done(ctx, "1", model.StatusFailure))

	pl := &model.Pipeline{ID: 1, Workflows: []*model.Workflow{
		{ID: 1, Name: "ci", State: model.StatusFailure},
		{ID: 2, Name: "notify", State: model.StatusPending, DependsOn: []string{"ci"}},
	}}

	mockStore := store_mocks.NewMockStore(t)

	skipped := SkipUnrunnableWorkflows(ctx, mockStore, q, pl)

	assert.Zero(t, skipped)
	assert.Equal(t, model.StatusPending, pl.Workflows[1].State, "notify must still be eligible to run")
	mockStore.AssertNotCalled(t, "WorkflowUpdate", mock.Anything)
}

// A dependent whose dependency succeeded is untouched.
func TestSkipUnrunnableWorkflowsLeavesHealthyChain(t *testing.T) {
	ctx := context.Background()
	q := queue.NewMemoryQueue(ctx)
	tasks := []*model.Task{
		{ID: "1"},
		{ID: "2", Dependencies: []string{"1"}, DepStatus: map[string]model.StatusValue{}},
	}
	assert.NoError(t, q.PushAtOnce(ctx, tasks))
	assert.NoError(t, q.Done(ctx, "1", model.StatusSuccess))

	pl := &model.Pipeline{ID: 1, Workflows: []*model.Workflow{
		{ID: 1, Name: "ci", State: model.StatusSuccess},
		{ID: 2, Name: "deploy", State: model.StatusPending, DependsOn: []string{"ci"}},
	}}

	mockStore := store_mocks.NewMockStore(t)

	assert.Zero(t, SkipUnrunnableWorkflows(ctx, mockStore, q, pl))
	assert.Equal(t, model.StatusPending, pl.Workflows[1].State)
}

// Only this pipeline's workflows are touched, never another pipeline's tasks
// that happen to share the queue.
func TestSkipUnrunnableWorkflowsIgnoresForeignTasks(t *testing.T) {
	ctx := context.Background()
	q := newWedgeQueue(ctx, t)

	// A pipeline that owns none of the queued task ids.
	pl := &model.Pipeline{ID: 99, Workflows: []*model.Workflow{
		{ID: 51, Name: "other", State: model.StatusPending, DependsOn: []string{"x"}},
	}}
	mockStore := store_mocks.NewMockStore(t)

	assert.Zero(t, SkipUnrunnableWorkflows(ctx, mockStore, q, pl))
	mockStore.AssertNotCalled(t, "WorkflowUpdate", mock.Anything)

	// and the wedged tasks are still there, untouched
	info := q.Info(ctx)
	assert.NotEmpty(t, append(append([]*model.Task{}, info.Pending...), info.WaitingOnDeps...))
}

// A store failure must not release the queue task: that would drop the work
// while leaving the pipeline hanging, which is worse than the original bug.
func TestSkipUnrunnableWorkflowsKeepsTaskWhenStoreFails(t *testing.T) {
	ctx := context.Background()
	q := newWedgeQueue(ctx, t)
	pl := wedgePipeline()

	mockStore := store_mocks.NewMockStore(t)
	mockStore.On("WorkflowUpdate", mock.Anything).Return(assert.AnError)

	assert.Zero(t, SkipUnrunnableWorkflows(ctx, mockStore, q, pl))

	info := q.Info(ctx)
	assert.NotEmpty(t, append(append([]*model.Task{}, info.Pending...), info.WaitingOnDeps...),
		"task must stay queued when its skip could not be persisted")
}

func TestSkipUnrunnableWorkflowsHandlesNilInputs(t *testing.T) {
	ctx := context.Background()
	mockStore := store_mocks.NewMockStore(t)
	assert.Zero(t, SkipUnrunnableWorkflows(ctx, mockStore, nil, wedgePipeline()))
	assert.Zero(t, SkipUnrunnableWorkflows(ctx, mockStore, queue.NewMemoryQueue(ctx), nil))
}

// The queue release failing must NOT undo the skip: the workflow is already
// settled in the store, and re-running it would double-execute. The task is
// logged and left, never resurrected.
func TestSkipUnrunnableWorkflowsSkipsEvenIfQueueReleaseFails(t *testing.T) {
	ctx := context.Background()
	pl := wedgePipeline()

	mockQueue := queue_mocks.NewMockQueue(t)
	mockQueue.On("Info", mock.Anything).Return(queue.InfoT{
		WaitingOnDeps: []*model.Task{
			{ID: "2", Dependencies: []string{"1"}, DepStatus: map[string]model.StatusValue{"1": model.StatusFailure}},
		},
	})
	mockQueue.On("Done", mock.Anything, "2", model.StatusSkipped).Return(assert.AnError)

	mockStore := store_mocks.NewMockStore(t)
	mockStore.On("WorkflowUpdate", mock.Anything).Return(nil)

	assert.Equal(t, 1, SkipUnrunnableWorkflows(ctx, mockStore, mockQueue, pl),
		"a failed queue release must still count as skipped")
	assert.Equal(t, model.StatusSkipped, pl.Workflows[1].State)
}

// An already-finished timestamp is preserved rather than overwritten.
func TestSkipUnrunnableWorkflowsPreservesExistingFinishedTime(t *testing.T) {
	ctx := context.Background()
	q := newWedgeQueue(ctx, t)
	pl := wedgePipeline()
	pl.Workflows[1].Finished = 12345

	mockStore := store_mocks.NewMockStore(t)
	mockStore.On("WorkflowUpdate", mock.Anything).Return(nil)

	SkipUnrunnableWorkflows(ctx, mockStore, q, pl)
	assert.Equal(t, int64(12345), pl.Workflows[1].Finished, "existing finish time must not be overwritten")
}
