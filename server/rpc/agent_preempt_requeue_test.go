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

package grpc

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/queue"
	store_mocks "go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
)

// preemptCIWorkflow models the 6041 shape: an idempotent CI workflow whose
// earlier steps succeeded and whose current step was killed by a preemption.
func preemptCIWorkflow() (*model.Workflow, []*model.Step) {
	wf := &model.Workflow{
		ID:         100,
		PipelineID: 200,
		Name:       "ci",
		AgentID:    42,
		State:      model.StatusRunning,
		Started:    1000,
	}
	steps := []*model.Step{
		{ID: 1, Name: "clone", State: model.StatusSuccess, Started: 1000, Finished: 1001},
		{ID: 2, Name: "test", State: model.StatusKilled, Started: 1001, ExitCode: -1, Error: "Canceled"},
		{ID: 3, Name: "lint", State: model.StatusSuccess, Started: 1001, Finished: 1002},
	}
	wf.Children = steps
	return wf, steps
}

// TestMaybeRequeueOnAgentShutdown_RequeuesIdempotentCI is the core #275 part-2
// case: a preempted CI workflow with already-succeeded steps is re-queued (not
// killed), and the FULL reset returns every step — including the successful
// ones — to pending so the fresh agent's re-reports aren't rejected as terminal.
func TestMaybeRequeueOnAgentShutdown_RequeuesIdempotentCI(t *testing.T) {
	wf, _ := preemptCIWorkflow()
	task := &model.Task{ID: "100", AgentID: 42}
	q := &stubQueue{info: queue.InfoT{Running: []*model.Task{task}}}

	s := store_mocks.NewMockStore(t)
	s.On("WorkflowUpdate", mock.MatchedBy(func(w *model.Workflow) bool {
		return w.ID == 100 && w.State == model.StatusPending && w.AgentID == 0 &&
			w.Started == 0 && w.Finished == 0 && w.Error == ""
	})).Return(nil)
	// Every step is reset to pending — including the two that succeeded.
	s.On("StepUpdate", mock.MatchedBy(func(st *model.Step) bool {
		return st.State == model.StatusPending && st.Started == 0 && st.Finished == 0 &&
			st.ExitCode == 0 && st.Error == ""
	})).Return(nil).Times(3)

	rpc := RPC{queue: q, store: s}
	ok := rpc.maybeRequeueOnAgentShutdown(context.Background(), wf, "100", false)

	assert.True(t, ok, "idempotent CI preemption must re-queue")
	assert.Equal(t, []*model.Task{task}, q.requeued, "task must be re-queued")
	assert.Empty(t, q.errored, "must not be killed")
	assert.Equal(t, 1, rpc.preemptRequeueCount(100), "budget consumed once")
	s.AssertExpectations(t)
}

// TestMaybeRequeueOnAgentShutdown_DeployClassNotRequeued: a deploy-class
// workflow (forced on-demand, not preemption-safe) must never auto-re-run.
func TestMaybeRequeueOnAgentShutdown_DeployClassNotRequeued(t *testing.T) {
	for _, name := range []string{"deploy", "promote-to-staging", "version-bump"} {
		t.Run(name, func(t *testing.T) {
			wf := &model.Workflow{ID: 100, Name: name, State: model.StatusRunning, Started: 1000}
			q := &stubQueue{info: queue.InfoT{Running: []*model.Task{{ID: "100"}}}}
			// No store interaction expected — guard returns before any reset.
			s := store_mocks.NewMockStore(t)

			rpc := RPC{queue: q, store: s}
			ok := rpc.maybeRequeueOnAgentShutdown(context.Background(), wf, "100", false)

			assert.False(t, ok, "deploy-class workflow must not re-queue")
			assert.Empty(t, q.requeued)
			s.AssertExpectations(t)
		})
	}
}

// TestMaybeRequeueOnAgentShutdown_TagEventNotRequeued: a tag-triggered workflow
// is a production release (ShouldForceOndemand true on isTagEvent) — never re-run.
func TestMaybeRequeueOnAgentShutdown_TagEventNotRequeued(t *testing.T) {
	wf := &model.Workflow{ID: 100, Name: "ci", State: model.StatusRunning, Started: 1000}
	q := &stubQueue{info: queue.InfoT{Running: []*model.Task{{ID: "100"}}}}
	s := store_mocks.NewMockStore(t)

	rpc := RPC{queue: q, store: s}
	ok := rpc.maybeRequeueOnAgentShutdown(context.Background(), wf, "100", true /* isTagEvent */)

	assert.False(t, ok, "tag-event workflow must not re-queue")
	assert.Empty(t, q.requeued)
}

// TestMaybeRequeueOnAgentShutdown_SyncBackRequeues: sync-back is deliberately
// NOT deploy-class (idempotent RELEASE_NOTES housekeeping) so it DOES self-heal.
func TestMaybeRequeueOnAgentShutdown_SyncBackRequeues(t *testing.T) {
	wf := &model.Workflow{ID: 100, Name: "sync-back", State: model.StatusRunning, Started: 1000}
	q := &stubQueue{info: queue.InfoT{Running: []*model.Task{{ID: "100"}}}}
	s := store_mocks.NewMockStore(t)
	s.On("WorkflowUpdate", mock.Anything).Return(nil)

	rpc := RPC{queue: q, store: s}
	ok := rpc.maybeRequeueOnAgentShutdown(context.Background(), wf, "100", false)

	assert.True(t, ok, "sync-back is idempotent and must self-heal")
	assert.Equal(t, []*model.Task{{ID: "100"}}, q.requeued)
}

// TestMaybeRequeueOnAgentShutdown_CapReached: past the per-workflow cap the
// self-heal falls through to the kill path (returns false), never looping.
func TestMaybeRequeueOnAgentShutdown_CapReached(t *testing.T) {
	wf := &model.Workflow{ID: 100, Name: "ci", State: model.StatusRunning, Started: 1000}
	q := &stubQueue{info: queue.InfoT{Running: []*model.Task{{ID: "100"}}}}
	s := store_mocks.NewMockStore(t)
	s.On("WorkflowUpdate", mock.Anything).Return(nil)

	rpc := RPC{queue: q, store: s}
	// Burn the budget up to the cap.
	for i := 0; i < maxPreemptRequeues; i++ {
		assert.True(t, rpc.maybeRequeueOnAgentShutdown(context.Background(), wf, "100", false),
			"requeue %d should succeed (under cap)", i+1)
	}
	// One past the cap: declined.
	ok := rpc.maybeRequeueOnAgentShutdown(context.Background(), wf, "100", false)
	assert.False(t, ok, "past cap must not re-queue")
	assert.Len(t, q.requeued, maxPreemptRequeues, "exactly cap re-queues, no more")
}

// TestMaybeRequeueOnAgentShutdown_TaskNotRunning: if the task is no longer in
// the running set there is nothing to re-queue — fall through to kill.
func TestMaybeRequeueOnAgentShutdown_TaskNotRunning(t *testing.T) {
	wf := &model.Workflow{ID: 100, Name: "ci", State: model.StatusRunning, Started: 1000}
	q := &stubQueue{info: queue.InfoT{Running: nil}} // task absent
	s := store_mocks.NewMockStore(t)

	rpc := RPC{queue: q, store: s}
	ok := rpc.maybeRequeueOnAgentShutdown(context.Background(), wf, "100", false)

	assert.False(t, ok, "missing task must not re-queue")
	assert.Empty(t, q.requeued)
	assert.Equal(t, 0, rpc.preemptRequeueCount(100), "no budget consumed on failure")
}

// TestMaybeRequeueOnAgentShutdown_RequeueFailsFallsBack: a Requeue error fails
// closed (returns false) rather than silently dropping the workflow.
func TestMaybeRequeueOnAgentShutdown_RequeueFailsFallsBack(t *testing.T) {
	wf := &model.Workflow{ID: 100, Name: "ci", State: model.StatusRunning, Started: 1000}
	q := &stubQueue{info: queue.InfoT{Running: []*model.Task{{ID: "100"}}}, requeueErr: errors.New("boom")}
	s := store_mocks.NewMockStore(t)
	s.On("WorkflowUpdate", mock.Anything).Return(nil)

	rpc := RPC{queue: q, store: s}
	ok := rpc.maybeRequeueOnAgentShutdown(context.Background(), wf, "100", false)

	assert.False(t, ok, "Requeue failure must fall through to kill")
}

// TestMaybeRequeueOnAgentShutdown_ResetFailsFallsBack: a store error during the
// full reset fails closed (returns false) — never silently drops the workflow.
func TestMaybeRequeueOnAgentShutdown_ResetFailsFallsBack(t *testing.T) {
	wf, _ := preemptCIWorkflow()
	q := &stubQueue{info: queue.InfoT{Running: []*model.Task{{ID: "100"}}}}
	s := store_mocks.NewMockStore(t)
	s.On("WorkflowUpdate", mock.Anything).Return(errors.New("db down"))

	rpc := RPC{queue: q, store: s}
	ok := rpc.maybeRequeueOnAgentShutdown(context.Background(), wf, "100", false)

	assert.False(t, ok, "reset failure must fall through to kill")
	assert.Empty(t, q.requeued, "no re-queue when reset failed")
	assert.Equal(t, 0, rpc.preemptRequeueCount(100), "no budget consumed on failed reset")
}

// TestResetWorkflowForFullRequeue_StepUpdateError surfaces a StepUpdate error.
func TestResetWorkflowForFullRequeue_StepUpdateError(t *testing.T) {
	wf, _ := preemptCIWorkflow()
	s := store_mocks.NewMockStore(t)
	s.On("WorkflowUpdate", mock.Anything).Return(nil)
	s.On("StepUpdate", mock.Anything).Return(errors.New("step write failed"))

	rpc := RPC{store: s}
	assert.Error(t, rpc.resetWorkflowForFullRequeue(wf))
}

// TestPreemptRequeueBudget_ClearResets verifies the budget map is released on
// terminal completion so a re-used workflow id starts fresh and does not leak.
func TestPreemptRequeueBudget_ClearResets(t *testing.T) {
	rpc := RPC{}
	rpc.incPreemptRequeue(100)
	rpc.incPreemptRequeue(100)
	assert.Equal(t, 2, rpc.preemptRequeueCount(100))

	rpc.clearPreemptRequeue(100)
	assert.Equal(t, 0, rpc.preemptRequeueCount(100), "cleared budget reads zero")

	// clear on an absent id (and nil map) is a safe no-op.
	rpc.clearPreemptRequeue(999)
	assert.Equal(t, 0, rpc.preemptRequeueCount(999))
}

// TestPreemptRequeueBudget_ConcurrentReadWrite exercises the budget map under
// concurrent inc/read/clear across many workflow ids — run with -race it proves
// the mutex serializes all access with no data race, panic, or deadlock.
func TestPreemptRequeueBudget_ConcurrentReadWrite(t *testing.T) {
	rpc := RPC{}
	var wg sync.WaitGroup
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				rpc.incPreemptRequeue(id)
				_ = rpc.preemptRequeueCount(id)
				if i%100 == 0 {
					rpc.clearPreemptRequeue(id)
				}
			}
		}(int64(w))
	}
	wg.Wait()
}

// TestResetWorkflowForFullRequeue resets the workflow and ALL steps regardless
// of prior state (distinct from the partial resetWorkflowForRequeue).
func TestResetWorkflowForFullRequeue(t *testing.T) {
	wf, _ := preemptCIWorkflow()
	s := store_mocks.NewMockStore(t)
	s.On("WorkflowUpdate", mock.MatchedBy(func(w *model.Workflow) bool {
		return w.State == model.StatusPending && w.AgentID == 0 && w.Started == 0
	})).Return(nil)
	s.On("StepUpdate", mock.MatchedBy(func(st *model.Step) bool {
		return st.State == model.StatusPending
	})).Return(nil).Times(3)

	rpc := RPC{store: s}
	assert.NoError(t, rpc.resetWorkflowForFullRequeue(wf))
	for _, st := range wf.Children {
		assert.Equal(t, model.StatusPending, st.State, "every step reset to pending")
		assert.Zero(t, st.ExitCode)
		assert.Empty(t, st.Error)
	}
	s.AssertExpectations(t)
}
