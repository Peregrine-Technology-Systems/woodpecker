package grpc

import (
	"context"
	"testing"

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
}

func (q *stubQueue) PushAtOnce(context.Context, []*model.Task) error { return nil }
func (q *stubQueue) Poll(context.Context, int64, queue.FilterFn) (*model.Task, error) {
	return nil, nil
}
func (q *stubQueue) Extend(context.Context, int64, string) error           { return nil }
func (q *stubQueue) Done(context.Context, string, model.StatusValue) error { return nil }
func (q *stubQueue) Error(context.Context, string, error) error            { return nil }
func (q *stubQueue) ErrorAtOnce(_ context.Context, ids []string, _ error) error {
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
		{ID: 100, State: model.StatusKilled, Finished: 1},
	}, nil)
	s.On("UpdatePipeline", mock.MatchedBy(func(p *model.Pipeline) bool {
		return p.Status == model.StatusKilled && p.Finished > 0
	})).Return(nil)
	s.On("GetRepo", int64(300)).Return(repo, nil)
	// updateForgeStatus calls GetUser — allow it to fail gracefully
	s.On("GetUser", mock.Anything).Return(nil, assert.AnError)

	rpc := RPC{queue: q, store: s}
	rpc.ReleaseAgentTasks(context.Background(), 42)

	s.AssertExpectations(t)
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
