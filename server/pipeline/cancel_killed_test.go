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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"go.woodpecker-ci.org/woodpecker/v3/server"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/pubsub"
	"go.woodpecker-ci.org/woodpecker/v3/server/queue"
	"go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
)

// #435 part 2, against the REAL memory queue: a pipeline is cancelled while
// compile runs. cleanup opted in with `when: status: [success, failure,
// killed]`; notify is `[success, failure]`, so it would run on a failure but
// did not opt in to a kill. Only cleanup may survive the cancel.

const (
	compileID = "10"
	cleanupID = "11"
	notifyID  = "12"
)

func everyTask(*model.Task) (bool, int) { return true, 1 }

func killFixture(t *testing.T, compileRunning bool) (queue.Queue, *mocks.MockStore, []*model.Workflow) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	q := queue.NewMemoryQueue(ctx)
	server.Config.Services.Queue = q
	server.Config.Services.Pubsub = pubsub.New()

	require.NoError(t, q.PushAtOnce(ctx, []*model.Task{
		{ID: compileID, PipelineID: 1, DepStatus: map[string]model.StatusValue{}},
		{
			ID: cleanupID, PipelineID: 1, Dependencies: []string{compileID},
			DepStatus: map[string]model.StatusValue{}, RunOn: []string{"success", "failure", "killed"},
		},
		{
			ID: notifyID, PipelineID: 1, Dependencies: []string{compileID},
			DepStatus: map[string]model.StatusValue{}, RunOn: []string{"success", "failure"},
		},
	}))
	compileState := model.StatusPending
	if compileRunning {
		got, err := q.Poll(ctx, 1, everyTask)
		require.NoError(t, err)
		require.Equal(t, compileID, got.ID)
		compileState = model.StatusRunning
	}

	pendingStep := func(id int64) []*model.Step { return []*model.Step{{ID: id, State: model.StatusPending}} }
	workflows := []*model.Workflow{
		{ID: 10, Name: "compile", State: compileState, DependsOn: []string{"wake"}},
		{ID: 11, Name: "cleanup", State: model.StatusPending, DependsOn: []string{"compile"}, Children: pendingStep(110)},
		{ID: 12, Name: "notify", State: model.StatusPending, DependsOn: []string{"compile"}, Children: pendingStep(120)},
	}
	s := mocks.NewMockStore(t)
	s.On("WorkflowGetTree", mock.Anything).Return(workflows, nil)
	return q, s, workflows
}

func expectSkipped(s *mocks.MockStore, workflowIDs, stepIDs []int64) {
	for _, id := range workflowIDs {
		s.On("WorkflowUpdate", mock.MatchedBy(func(w *model.Workflow) bool { return w.ID == id })).Return(nil).Once()
	}
	for _, id := range stepIDs {
		s.On("StepUpdate", mock.MatchedBy(func(st *model.Step) bool { return st.ID == id })).Return(nil).Once()
	}
}

// nextTask polls the way an agent would, within d. nil means nothing came.
func nextTask(t *testing.T, q queue.Queue, d time.Duration) *model.Task {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	got, _ := q.Poll(ctx, 2, everyTask)
	return got
}

func TestCancel_KilledOptInWorkflowRunsAfterTheCancel(t *testing.T) {
	q, s, _ := killFixture(t, true)
	expectSkipped(s, []int64{12}, []int64{120})
	var saved *model.Pipeline
	s.On("UpdatePipeline", mock.Anything).Return(nil).Run(func(a mock.Arguments) { saved = a.Get(0).(*model.Pipeline) })

	pl := &model.Pipeline{ID: 1, Status: model.StatusRunning, Started: 1}
	err := Cancel(context.Background(), nil, s, &model.Repo{FullName: "org/fork"}, &model.User{}, pl,
		&model.CancelInfo{CanceledByUser: "ops"}, false)
	require.NoError(t, err)

	require.NotNil(t, saved)
	assert.Equal(t, model.StatusKilled, saved.Status, "a user cancel still reads as killed")
	assert.Equal(t, []int64{11}, saved.CancelInfo.KillHandlers, "the kept workflow is recorded")

	got := nextTask(t, q, 2*time.Second)
	require.NotNil(t, got, "the opted-in cleanup must still be dispatched after the cancel")
	assert.Equal(t, cleanupID, got.ID)
	assert.True(t, got.ShouldRun(), "and run, not be skipped, with its dependency killed")
	assert.Equal(t, model.StatusKilled, got.DepStatus[compileID])

	assert.Nil(t, nextTask(t, q, 300*time.Millisecond), "notify did not opt in: it must not be dispatched")
}

// ENG's third case: a successor push superseding the pipeline is unchanged.
// The opt-in is for a deliberate cancel, not for every superseded push.
func TestCancel_SupersedeStillEvictsTheKilledOptIn(t *testing.T) {
	q, s, _ := killFixture(t, true)
	expectSkipped(s, []int64{11, 12}, []int64{110, 120})
	var saved *model.Pipeline
	s.On("UpdatePipeline", mock.Anything).Return(nil).Run(func(a mock.Arguments) { saved = a.Get(0).(*model.Pipeline) })

	pl := &model.Pipeline{ID: 1, Status: model.StatusRunning, Started: 1}
	err := Cancel(context.Background(), nil, s, &model.Repo{FullName: "org/fork"}, &model.User{}, pl,
		&model.CancelInfo{SupersededBy: 2}, true)
	require.NoError(t, err)

	require.NotNil(t, saved)
	assert.Equal(t, model.StatusSuperseded, saved.Status)
	assert.Empty(t, saved.CancelInfo.KillHandlers)
	assert.Nil(t, nextTask(t, q, 300*time.Millisecond), "nothing survives a supersede")
}

// Nothing ever started, so there is nothing to clean up: unchanged.
func TestCancel_PendingOnlyStillEvictsTheKilledOptIn(t *testing.T) {
	q, s, _ := killFixture(t, false)
	expectSkipped(s, []int64{10, 11, 12}, []int64{110, 120})
	var saved *model.Pipeline
	s.On("UpdatePipeline", mock.Anything).Return(nil).Run(func(a mock.Arguments) { saved = a.Get(0).(*model.Pipeline) })

	pl := &model.Pipeline{ID: 1, Status: model.StatusPending}
	err := Cancel(context.Background(), nil, s, &model.Repo{FullName: "org/fork"}, &model.User{}, pl,
		&model.CancelInfo{CanceledByUser: "ops"}, false)
	require.NoError(t, err)

	require.NotNil(t, saved)
	assert.Equal(t, model.StatusCanceled, saved.Status)
	assert.Empty(t, saved.CancelInfo.KillHandlers)
	assert.Nil(t, nextTask(t, q, 300*time.Millisecond), "nothing survives a pending-only cancel")
}

// The kept workflow's own success must not turn a killed pipeline partial; a
// failed handler is counted as any workflow is. Without kill handlers nothing changes.
func TestFinalPipelineStatus_KillHandlers(t *testing.T) {
	wf := func(id int64, s model.StatusValue) *model.Workflow { return &model.Workflow{ID: id, State: s} }
	killedWith := func(cleanup model.StatusValue, handlers []int64) *model.Pipeline {
		return &model.Pipeline{
			CancelInfo: &model.CancelInfo{KillHandlers: handlers},
			Workflows:  []*model.Workflow{wf(10, model.StatusKilled), wf(11, cleanup), wf(12, model.StatusSkipped)},
		}
	}
	assert.Equal(t, model.StatusKilled, FinalPipelineStatus(killedWith(model.StatusSuccess, []int64{11})))
	failed := killedWith(model.StatusFailure, []int64{11})
	assert.Equal(t, PipelineStatus(failed.Workflows), FinalPipelineStatus(failed), "a failed handler is counted as usual")
	assert.Equal(t, model.StatusPartial, FinalPipelineStatus(killedWith(model.StatusSuccess, nil)), "unchanged without the opt-in")
	assert.Equal(t, model.StatusRunning, FinalPipelineStatus(killedWith(model.StatusRunning, []int64{11})), "still running")
	assert.Equal(t, model.StatusSuccess, FinalPipelineStatus(&model.Pipeline{Workflows: []*model.Workflow{wf(1, model.StatusSuccess)}}))
}
