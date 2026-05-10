package grpc

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/queue"
	store_mocks "go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
)

// stubQueue implements queue.Queue with minimal stubs for testing.
type stubQueue struct {
	info          queue.InfoT
	errorAtOnce   error
	errorAtOnceFn func(ids []string)
	pushed        []*model.Task // tasks passed to PushAtOnce
	errored       []string      // task IDs passed to ErrorAtOnce
}

func (q *stubQueue) PushAtOnce(_ context.Context, tasks []*model.Task) error {
	q.pushed = append(q.pushed, tasks...)
	return nil
}
func (q *stubQueue) Poll(context.Context, int64, queue.FilterFn) (*model.Task, error) {
	return nil, nil
}
func (q *stubQueue) Extend(context.Context, int64, string) error           { return nil }
func (q *stubQueue) Done(context.Context, string, model.StatusValue) error { return nil }
func (q *stubQueue) Error(context.Context, string, error) error            { return nil }
func (q *stubQueue) ErrorAtOnce(_ context.Context, ids []string, _ error) error {
	q.errored = append(q.errored, ids...)
	if q.errorAtOnceFn != nil {
		q.errorAtOnceFn(ids)
	}
	return q.errorAtOnce
}
func (q *stubQueue) Wait(context.Context, string) error { return nil }
func (q *stubQueue) Info(context.Context) queue.InfoT   { return q.info }
func (q *stubQueue) Pause()                             {}
func (q *stubQueue) Resume()                            {}
func (q *stubQueue) KickAgentWorkers(int64)             {}
func (q *stubQueue) SetDispatchHook(queue.DispatchFunc) {}
func (q *stubQueue) UpdatePriority(string, int64) (int64, error) {
	return 0, queue.ErrNotFound
}

func TestReleaseAgentTasks_NoOrphanedTasks(t *testing.T) {
	q := &stubQueue{info: queue.InfoT{}}
	s := store_mocks.NewMockStore(t)
	rpc := RPC{queue: q, store: s}

	// Should return immediately without any store calls
	rpc.ReleaseAgentTasks(context.Background(), 99)
}

func TestReleaseAgentTasks_KillsWorkflowAndPipeline(t *testing.T) {
	// Simulate agent 42 running workflow "100"
	q := &stubQueue{
		info: queue.InfoT{
			Running: []*model.Task{
				{ID: "100", AgentID: 42},
			},
		},
	}

	s := store_mocks.NewMockStore(t)

	step1 := &model.Step{ID: 1, State: model.StatusRunning}
	step2 := &model.Step{ID: 2, State: model.StatusPending}

	workflow := &model.Workflow{
		ID:         100,
		PipelineID: 200,
		State:      model.StatusRunning,
		Started:    time.Now().Unix(), // Started > 0 → kill path, not re-queue (#72)
	}

	pipelineModel := &model.Pipeline{
		ID:     200,
		RepoID: 300,
		Status: model.StatusRunning,
	}

	repo := &model.Repo{ID: 300, FullName: "test/repo"}

	// Mock store calls in order
	s.On("WorkflowLoad", int64(100)).Return(workflow, nil)
	s.On("StepListFromWorkflowFind", workflow).Return([]*model.Step{step1, step2}, nil)
	s.On("StepUpdate", mock.MatchedBy(func(step *model.Step) bool {
		return step.State == model.StatusKilled && step.Error == "agent disconnected"
	})).Return(nil)
	s.On("WorkflowUpdate", mock.MatchedBy(func(w *model.Workflow) bool {
		return w.State == model.StatusKilled && w.Error == "agent disconnected" && w.Finished > 0
	})).Return(nil)
	s.On("GetPipeline", int64(200)).Return(pipelineModel, nil)
	s.On("WorkflowGetTree", pipelineModel).Return([]*model.Workflow{
		// Error="agent disconnected" matches Workflow.KilledByAgentDisconnect()
		// which UpdateStatusToDone uses to derive KillReason="agent_disconnect".
		{ID: 100, State: model.StatusKilled, Finished: 1, Error: "agent disconnected"},
	}, nil)
	s.On("UpdatePipeline", mock.MatchedBy(func(p *model.Pipeline) bool {
		// #202: agent-disconnect kills are stamped with reason + killed_at
		return p.Status == model.StatusKilled && p.Finished > 0 &&
			p.KillReason == "agent_disconnect" && p.KilledAt > 0
	})).Return(nil)
	s.On("GetRepo", int64(300)).Return(repo, nil)
	// fork#44: updateForgeStatus now early-returns BEFORE GetUser when the
	// workflow was killed by agent disconnect (the case under test). The
	// previous behavior was to GetUser → forge.Status with state=errored,
	// which cascaded into branch-protection blocks. We now expect zero
	// GetUser calls on this path.

	rpc := RPC{queue: q, store: s}
	rpc.ReleaseAgentTasks(context.Background(), 42)

	s.AssertExpectations(t)
	s.AssertNotCalled(t, "GetUser", mock.Anything)
}

func TestReleaseAgentTasks_OnlyReleasesMatchingAgent(t *testing.T) {
	// Agent 42 has a task, agent 99 does not — releasing 99 should be a no-op
	q := &stubQueue{
		info: queue.InfoT{
			Running: []*model.Task{
				{ID: "100", AgentID: 42},
			},
		},
	}

	s := store_mocks.NewMockStore(t)
	rpc := RPC{queue: q, store: s}

	rpc.ReleaseAgentTasks(context.Background(), 99)

	// No store calls should have been made
	s.AssertNotCalled(t, "WorkflowLoad", mock.Anything)
}

// TestReleaseAgentTasks_RequeueClaimed verifies that a task claimed by a
// disconnecting agent but not yet started (workflow.Started == 0) is
// re-queued instead of killed. Regression test for fork#72.
func TestReleaseAgentTasks_RequeueClaimed(t *testing.T) {
	task := &model.Task{ID: "100", AgentID: 42}
	q := &stubQueue{
		info: queue.InfoT{
			Running: []*model.Task{task},
		},
	}

	// workflow.Started == 0: agent claimed but never began executing
	workflow := &model.Workflow{
		ID:         100,
		PipelineID: 200,
		State:      model.StatusPending,
		Started:    0,
	}

	s := store_mocks.NewMockStore(t)
	s.On("WorkflowLoad", int64(100)).Return(workflow, nil)

	rpc := RPC{queue: q, store: s}
	rpc.ReleaseAgentTasks(context.Background(), 42)

	// Task must be re-queued, not killed
	assert.Equal(t, []*model.Task{task}, q.pushed, "claimed task must be re-queued")
	assert.Empty(t, q.errored, "claimed task must not be killed")

	// No DB kill updates — the workflow never ran
	s.AssertNotCalled(t, "StepListFromWorkflowFind", mock.Anything)
	s.AssertNotCalled(t, "WorkflowUpdate", mock.Anything)
}
