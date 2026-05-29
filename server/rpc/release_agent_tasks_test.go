package grpc

import (
	"context"
	"errors"
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
	requeued      []*model.Task // tasks passed to Requeue (#225)
	errored       []string      // task IDs passed to ErrorAtOnce
	requeueErr    error         // error to return from Requeue (default nil)
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
func (q *stubQueue) Requeue(_ context.Context, task *model.Task) error {
	if q.requeueErr != nil {
		return q.requeueErr
	}
	q.requeued = append(q.requeued, task)
	return nil
}
func (q *stubQueue) Wait(context.Context, string) error               { return nil }
func (q *stubQueue) Info(context.Context) queue.InfoT                 { return q.info }
func (q *stubQueue) Pause()                                           {}
func (q *stubQueue) Resume()                                          {}
func (q *stubQueue) KickAgentWorkers(int64)                           {}
func (q *stubQueue) SetDispatchHook(queue.DispatchFunc)               {}
func (q *stubQueue) SetAgentLivenessFn(func(int64) bool, func(int64)) {}
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

	// step1 did real work: started and produced log output. That makes the
	// workflow unsafe to re-queue (#235 F3) → kill path.
	step1 := &model.Step{ID: 1, State: model.StatusRunning, Started: time.Now().Unix()}
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
	// #235 F3: stepDidRealWork consults logs for a running step with no real
	// exit code; one log line proves work was done → kill, not re-queue.
	s.On("LogFind", step1).Return([]*model.LogEntry{{StepID: 1, Line: 1}}, nil)
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
// disconnecting agent but not yet started (workflow.Started == 0, all steps
// pending) is re-queued instead of killed. Regression test for fork#72.
func TestReleaseAgentTasks_RequeueClaimed(t *testing.T) {
	task := &model.Task{ID: "100", AgentID: 42}
	q := &stubQueue{
		info: queue.InfoT{
			Running: []*model.Task{task},
		},
	}

	// workflow.Started == 0: agent claimed but never called Init
	workflow := &model.Workflow{
		ID:         100,
		PipelineID: 200,
		State:      model.StatusPending,
		Started:    0,
	}
	pendingStep := &model.Step{ID: 1, State: model.StatusPending, Started: 0}

	s := store_mocks.NewMockStore(t)
	s.On("WorkflowLoad", int64(100)).Return(workflow, nil)
	s.On("StepListFromWorkflowFind", workflow).Return([]*model.Step{pendingStep}, nil)

	rpc := RPC{queue: q, store: s}
	rpc.ReleaseAgentTasks(context.Background(), 42)

	// Task must be re-queued via Requeue, not killed
	assert.Equal(t, []*model.Task{task}, q.requeued, "claimed task must be re-queued")
	assert.Empty(t, q.errored, "claimed task must not be killed")
	assert.Empty(t, q.pushed, "PushAtOnce must not be used — Requeue is the correct path")

	// No DB kill updates and no Init-reset (Started == 0, no reset needed)
	s.AssertNotCalled(t, "WorkflowUpdate", mock.Anything)
	s.AssertExpectations(t)
}

// TestReleaseAgentTasks_RequeueAfterInit verifies the #224 fix: a workflow
// whose agent called Init (setting Started > 0) but then disconnected before
// executing any step is re-queued (not killed), and its DB state is reset to
// pending so the UI shows the correct state.
func TestReleaseAgentTasks_RequeueAfterInit(t *testing.T) {
	task := &model.Task{ID: "200", AgentID: 55}
	q := &stubQueue{
		info: queue.InfoT{
			Running: []*model.Task{task},
		},
	}

	// Init was called: Started > 0, State = running, but no step has executed
	workflow := &model.Workflow{
		ID:         200,
		PipelineID: 300,
		AgentID:    55,
		State:      model.StatusRunning,
		Started:    time.Now().Unix(),
	}
	pendingStep := &model.Step{ID: 10, State: model.StatusPending, Started: 0}

	s := store_mocks.NewMockStore(t)
	s.On("WorkflowLoad", int64(200)).Return(workflow, nil)
	s.On("StepListFromWorkflowFind", workflow).Return([]*model.Step{pendingStep}, nil)
	s.On("WorkflowUpdate", mock.MatchedBy(func(w *model.Workflow) bool {
		return w.ID == 200 && w.State == model.StatusPending && w.AgentID == 0 && w.Started == 0
	})).Return(nil)

	rpc := RPC{queue: q, store: s}
	rpc.ReleaseAgentTasks(context.Background(), 55)

	assert.Equal(t, []*model.Task{task}, q.requeued, "Init-then-disconnect task must be re-queued")
	assert.Empty(t, q.errored, "Init-then-disconnect task must not be killed")
	s.AssertExpectations(t)
}

// TestReleaseAgentTasks_RequeuePhantomStartedStep verifies the #235 F3 widening
// (#1327): a workflow whose step was claimed and stamped Started but produced no
// output and never returned a real exit code — the "agent ack'd then vanished"
// shape (e.g. spot VM preempted seconds after dispatch-ack) — is re-queued, and
// the phantom step is reset to pending so the new agent's updates aren't
// rejected as terminal-state transitions.
func TestReleaseAgentTasks_RequeuePhantomStartedStep(t *testing.T) {
	task := &model.Task{ID: "100", AgentID: 42}
	q := &stubQueue{info: queue.InfoT{Running: []*model.Task{task}}}

	workflow := &model.Workflow{
		ID:         100,
		PipelineID: 200,
		AgentID:    42,
		State:      model.StatusRunning,
		Started:    time.Now().Unix(),
	}
	// Phantom step: started, but failure with exit_code=-1 and no logs.
	phantom := &model.Step{ID: 1, State: model.StatusFailure, Started: time.Now().Unix(), ExitCode: -1}

	s := store_mocks.NewMockStore(t)
	s.On("WorkflowLoad", int64(100)).Return(workflow, nil)
	s.On("StepListFromWorkflowFind", workflow).Return([]*model.Step{phantom}, nil)
	s.On("LogFind", phantom).Return([]*model.LogEntry{}, nil) // no output → no real work
	s.On("WorkflowUpdate", mock.MatchedBy(func(w *model.Workflow) bool {
		return w.ID == 100 && w.State == model.StatusPending && w.AgentID == 0 && w.Started == 0
	})).Return(nil)
	s.On("StepUpdate", mock.MatchedBy(func(step *model.Step) bool {
		return step.ID == 1 && step.State == model.StatusPending && step.Started == 0 && step.ExitCode == 0
	})).Return(nil)

	rpc := RPC{queue: q, store: s}
	rpc.ReleaseAgentTasks(context.Background(), 42)

	assert.Equal(t, []*model.Task{task}, q.requeued, "phantom-started task must be re-queued (#1327)")
	assert.Empty(t, q.errored, "phantom-started task must not be killed")
	s.AssertExpectations(t)
}

// TestReleaseAgentTasks_KillsWhenStepRanAndExited verifies the F3 widening stays
// conservative: a step that ran and returned a real (positive) exit code did
// observable work, so its workflow is killed — never re-queued — to avoid
// duplicating side effects.
func TestReleaseAgentTasks_KillsWhenStepRanAndExited(t *testing.T) {
	task := &model.Task{ID: "100", AgentID: 42}
	q := &stubQueue{info: queue.InfoT{Running: []*model.Task{task}}}

	workflow := &model.Workflow{ID: 100, PipelineID: 200, State: model.StatusRunning, Started: time.Now().Unix()}
	// ranStep is first so its positive exit code short-circuits the partition
	// before any LogFind. The pending sibling makes anyKilled true in the kill
	// loop, so the workflow is stamped agent-disconnect and forge status
	// early-returns (no GetUser) — same path as KillsWorkflowAndPipeline.
	ranStep := &model.Step{ID: 1, State: model.StatusFailure, Started: time.Now().Unix(), ExitCode: 1}
	pendingStep := &model.Step{ID: 2, State: model.StatusPending}

	s := store_mocks.NewMockStore(t)
	s.On("WorkflowLoad", int64(100)).Return(workflow, nil)
	s.On("StepListFromWorkflowFind", workflow).Return([]*model.Step{ranStep, pendingStep}, nil)
	s.On("StepUpdate", mock.Anything).Return(nil)
	s.On("WorkflowUpdate", mock.Anything).Return(nil)
	s.On("GetPipeline", int64(200)).Return(&model.Pipeline{ID: 200, RepoID: 300, Status: model.StatusRunning}, nil)
	s.On("WorkflowGetTree", mock.Anything).Return([]*model.Workflow{
		{ID: 100, State: model.StatusKilled, Finished: 1, Error: "agent disconnected"},
	}, nil)
	s.On("UpdatePipeline", mock.Anything).Return(nil)
	s.On("GetRepo", int64(300)).Return(&model.Repo{ID: 300, FullName: "test/repo"}, nil)

	rpc := RPC{queue: q, store: s}
	rpc.ReleaseAgentTasks(context.Background(), 42)

	assert.Contains(t, q.errored, "100", "a step that ran and exited must force kill, not re-queue")
	assert.Empty(t, q.requeued, "step that did real work must not be re-queued")
	s.AssertNotCalled(t, "LogFind", mock.Anything) // positive exit code short-circuits the log check
}

// TestReleaseAgentTasks_RequeueFailureFallsBackToKill verifies that when
// Requeue returns an error the task is killed rather than silently lost (#225).
func TestReleaseAgentTasks_RequeueFailureFallsBackToKill(t *testing.T) {
	task := &model.Task{ID: "300", AgentID: 77}
	q := &stubQueue{
		info: queue.InfoT{
			Running: []*model.Task{task},
		},
		requeueErr: errors.New("queue full"),
	}

	workflow := &model.Workflow{
		ID:         300,
		PipelineID: 400,
		State:      model.StatusPending,
		Started:    0,
	}
	pendingStep := &model.Step{ID: 20, State: model.StatusPending, Started: 0}

	s := store_mocks.NewMockStore(t)
	s.On("WorkflowLoad", int64(300)).Return(workflow, nil)
	s.On("StepListFromWorkflowFind", workflow).Return([]*model.Step{pendingStep}, nil)
	// Kill path: workflow was in workflowCache via nil-task guard? No — task IS
	// in running map, so it goes through re-queue path. On Requeue failure it
	// moves to toKill. Kill loop re-loads workflow from DB (not in cache).
	s.On("WorkflowLoad", int64(300)).Return(workflow, nil)
	s.On("StepListFromWorkflowFind", workflow).Return([]*model.Step{pendingStep}, nil)
	s.On("StepUpdate", mock.MatchedBy(func(step *model.Step) bool {
		return step.State == model.StatusKilled
	})).Return(nil)
	s.On("WorkflowUpdate", mock.MatchedBy(func(w *model.Workflow) bool {
		return w.State == model.StatusKilled
	})).Return(nil)
	s.On("GetPipeline", int64(400)).Return(&model.Pipeline{ID: 400, RepoID: 500, Status: model.StatusRunning}, nil)
	s.On("WorkflowGetTree", mock.Anything).Return([]*model.Workflow{
		{ID: 300, State: model.StatusKilled, Finished: 1, Error: "agent disconnected"},
	}, nil)
	s.On("UpdatePipeline", mock.Anything).Return(nil)
	s.On("GetRepo", int64(500)).Return(&model.Repo{ID: 500, FullName: "test/repo"}, nil)

	rpc := RPC{queue: q, store: s}
	rpc.ReleaseAgentTasks(context.Background(), 77)

	assert.Contains(t, q.errored, "300", "Requeue failure must fall back to kill via ErrorAtOnce")
}
