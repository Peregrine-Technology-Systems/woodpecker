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

package queue

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	storemocks "go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
	"go.woodpecker-ci.org/woodpecker/v3/server/store/types"
)

func TestWithTaskStore_EvictsZombieTasks(t *testing.T) {
	tasks := []*model.Task{
		{ID: "alive-1", PipelineID: 1},
		{ID: "alive-2", PipelineID: 2},
		{ID: "zombie-killed", PipelineID: 10},
		{ID: "zombie-success", PipelineID: 11},
		{ID: "zombie-error", PipelineID: 12},
		{ID: "zombie-no-pipeline", PipelineID: 99},
	}

	s := storemocks.NewMockStore(t)
	s.On("TaskList").Return(tasks, nil)
	s.On("GetPipeline", int64(1)).Return(&model.Pipeline{ID: 1, Status: model.StatusRunning}, nil)
	s.On("GetPipeline", int64(2)).Return(&model.Pipeline{ID: 2, Status: model.StatusPending}, nil)
	s.On("GetPipeline", int64(10)).Return(&model.Pipeline{ID: 10, Status: model.StatusKilled}, nil)
	s.On("GetPipeline", int64(11)).Return(&model.Pipeline{ID: 11, Status: model.StatusSuccess}, nil)
	s.On("GetPipeline", int64(12)).Return(&model.Pipeline{ID: 12, Status: model.StatusError}, nil)
	s.On("GetPipeline", int64(99)).Return(nil, fmt.Errorf("not found"))
	s.On("TaskDelete", mock.AnythingOfType("string")).Return(nil)

	ctx := context.Background()
	inner, _ := New(ctx, Config{Backend: TypeMemory})
	WithTaskStore(ctx, inner, s)

	// Verify zombie tasks were deleted
	for _, zombie := range []string{"zombie-killed", "zombie-success", "zombie-error", "zombie-no-pipeline"} {
		s.AssertCalled(t, "TaskDelete", zombie)
	}
	// Verify alive tasks were NOT deleted
	s.AssertNotCalled(t, "TaskDelete", "alive-1")
	s.AssertNotCalled(t, "TaskDelete", "alive-2")
}

func TestIsTerminalPipeline(t *testing.T) {
	terminal := []model.StatusValue{
		model.StatusSuccess, model.StatusFailure, model.StatusKilled,
		model.StatusError, model.StatusDeclined, model.StatusSkipped,
		model.StatusSuperseded, model.StatusCanceled,
	}
	active := []model.StatusValue{
		model.StatusRunning, model.StatusPending, model.StatusCreated, model.StatusBlocked,
	}
	for _, s := range terminal {
		assert.True(t, isTerminalPipeline(s), "expected terminal: %s", s)
	}
	for _, s := range active {
		assert.False(t, isTerminalPipeline(s), "expected active: %s", s)
	}
}

// freshPersistentQueue builds an empty persistent queue against a mock store
// for the wrapper-method tests below.
func freshPersistentQueue(t *testing.T) (*persistentQueue, *storemocks.MockStore, context.Context, context.CancelFunc) {
	t.Helper()
	s := storemocks.NewMockStore(t)
	s.On("TaskList").Return([]*model.Task{}, nil).Maybe()
	ctx, cancel := context.WithCancel(context.Background())
	inner, _ := New(ctx, Config{Backend: TypeMemory})
	pq := WithTaskStore(ctx, inner, s).(*persistentQueue)
	return pq, s, ctx, cancel
}

func TestPersistentQueue_PushAtOnce_PersistsThenQueues(t *testing.T) {
	pq, s, ctx, cancel := freshPersistentQueue(t)
	defer cancel()

	task := &model.Task{ID: "p1"}
	s.On("TaskInsert", task).Return(nil)

	assert.NoError(t, pq.PushAtOnce(ctx, []*model.Task{task}))
	s.AssertCalled(t, "TaskInsert", task)
}

func TestPersistentQueue_PushAtOnce_StoreErrorAborts(t *testing.T) {
	pq, s, ctx, cancel := freshPersistentQueue(t)
	defer cancel()

	task := &model.Task{ID: "p2"}
	s.On("TaskInsert", task).Return(fmt.Errorf("disk full"))

	err := pq.PushAtOnce(ctx, []*model.Task{task})
	assert.Error(t, err, "store insert failure should propagate")
}

func TestPersistentQueue_Poll_DeletesFromStore(t *testing.T) {
	pq, s, ctx, cancel := freshPersistentQueue(t)
	defer cancel()

	task := &model.Task{ID: "polled"}
	s.On("TaskInsert", task).Return(nil)
	s.On("TaskDelete", "polled").Return(nil)

	assert.NoError(t, pq.PushAtOnce(ctx, []*model.Task{task}))
	got, err := pq.Poll(ctx, 1, func(*model.Task) (bool, int) { return true, 1 })
	assert.NoError(t, err)
	assert.Equal(t, "polled", got.ID)
	s.AssertCalled(t, "TaskDelete", "polled")
}

func TestPersistentQueue_Error_DeletesFromStore(t *testing.T) {
	pq, s, ctx, cancel := freshPersistentQueue(t)
	defer cancel()

	task := &model.Task{ID: "errored"}
	s.On("TaskInsert", task).Return(nil)
	s.On("TaskDelete", "errored").Return(nil)

	assert.NoError(t, pq.PushAtOnce(ctx, []*model.Task{task}))
	_, _ = pq.Poll(ctx, 1, func(*model.Task) (bool, int) { return true, 1 })

	assert.NoError(t, pq.Error(ctx, "errored", fmt.Errorf("agent crashed")))
	s.AssertCalled(t, "TaskDelete", "errored")
}

func TestPersistentQueue_ErrorAtOnce_DeletesAll(t *testing.T) {
	pq, s, ctx, cancel := freshPersistentQueue(t)
	defer cancel()

	tasks := []*model.Task{{ID: "ea1"}, {ID: "ea2"}}
	for _, task := range tasks {
		s.On("TaskInsert", task).Return(nil)
		s.On("TaskDelete", task.ID).Return(nil)
	}

	assert.NoError(t, pq.PushAtOnce(ctx, tasks))
	_, _ = pq.Poll(ctx, 1, func(*model.Task) (bool, int) { return true, 1 })
	_, _ = pq.Poll(ctx, 1, func(*model.Task) (bool, int) { return true, 1 })

	assert.NoError(t, pq.ErrorAtOnce(ctx, []string{"ea1", "ea2"}, fmt.Errorf("batch fail")))
	s.AssertCalled(t, "TaskDelete", "ea1")
	s.AssertCalled(t, "TaskDelete", "ea2")
}

// TestPersistentQueue_UpdatePriority_SyncsBothLayers locks in the #47
// contract: the wrapper updates the in-memory queue first (fails fast on
// "task not pending") and only persists to DB after the in-memory mutation
// succeeds. Returns the old priority on success.
func TestPersistentQueue_UpdatePriority_SyncsBothLayers(t *testing.T) {
	pq, s, ctx, cancel := freshPersistentQueue(t)
	defer cancel()

	task := &model.Task{ID: "up1", Priority: 0}
	s.On("TaskInsert", task).Return(nil)
	s.On("UpdateTaskPriority", "up1", int64(7)).Return(nil)

	assert.NoError(t, pq.PushAtOnce(ctx, []*model.Task{task}))

	old, err := pq.UpdatePriority("up1", 7)
	assert.NoError(t, err)
	assert.EqualValues(t, 0, old, "should return previous priority")
	s.AssertCalled(t, "UpdateTaskPriority", "up1", int64(7))
}

// TestPersistentQueue_UpdatePriority_QueueErrorSkipsDB locks in the gate:
// if the in-memory mutation fails (task not pending), the DB is NOT
// touched. This matters for SOC 2 — we don't want a divergence where DB
// shows a priority change that didn't actually happen at dispatch time.
func TestPersistentQueue_UpdatePriority_QueueErrorSkipsDB(t *testing.T) {
	pq, s, _, cancel := freshPersistentQueue(t)
	defer cancel()

	_, err := pq.UpdatePriority("nonexistent", 5)
	assert.ErrorIs(t, err, ErrNotFound)
	s.AssertNotCalled(t, "UpdateTaskPriority")
}

// TestPersistentQueue_UpdatePriority_DBErrorReturnsErr — if in-memory
// succeeded but DB write fails, we return the DB error. The in-memory
// mutation is intentionally NOT rolled back; restart will re-seed from DB
// and the divergence resolves itself. The error is logged loudly per the
// implementation.
func TestPersistentQueue_UpdatePriority_DBErrorReturnsErr(t *testing.T) {
	pq, s, ctx, cancel := freshPersistentQueue(t)
	defer cancel()

	task := &model.Task{ID: "up2", Priority: 0}
	s.On("TaskInsert", task).Return(nil)
	s.On("UpdateTaskPriority", "up2", int64(3)).Return(fmt.Errorf("disk full"))

	assert.NoError(t, pq.PushAtOnce(ctx, []*model.Task{task}))

	old, err := pq.UpdatePriority("up2", 3)
	assert.Error(t, err)
	assert.EqualValues(t, 0, old, "old priority should still be reported even on DB failure")
}

// TestPersistentQueue_Poll_StoreDeleteErrorDoesntFailPoll covers the log
// branch in Poll where TaskDelete returns an error after a successful poll.
// The poll itself must still succeed — agent gets the task — and the store
// drift is tolerated until next restart.
func TestPersistentQueue_Poll_StoreDeleteErrorDoesntFailPoll(t *testing.T) {
	pq, s, ctx, cancel := freshPersistentQueue(t)
	defer cancel()

	task := &model.Task{ID: "delfail"}
	s.On("TaskInsert", task).Return(nil)
	s.On("TaskDelete", "delfail").Return(fmt.Errorf("disk read-only"))

	assert.NoError(t, pq.PushAtOnce(ctx, []*model.Task{task}))
	got, err := pq.Poll(ctx, 1, func(*model.Task) (bool, int) { return true, 1 })
	assert.NoError(t, err, "poll succeeds even if store delete fails (logged, not raised)")
	assert.Equal(t, "delfail", got.ID)
}

// TestPersistentQueue_Error_StoreDeleteRecordNotExist covers the lenient
// path: TaskDelete returning ErrRecordNotExist is treated as "already gone"
// and downgraded to debug log, not an error.
func TestPersistentQueue_Error_StoreDeleteRecordNotExist(t *testing.T) {
	pq, s, ctx, cancel := freshPersistentQueue(t)
	defer cancel()

	task := &model.Task{ID: "err-gone"}
	s.On("TaskInsert", task).Return(nil)
	s.On("TaskDelete", "err-gone").Return(types.ErrRecordNotExist).Once()

	assert.NoError(t, pq.PushAtOnce(ctx, []*model.Task{task}))
	_, _ = pq.Poll(ctx, 1, func(*model.Task) (bool, int) { return true, 1 })

	// Re-issue Error on same id; TaskDelete returns "already gone".
	// Have to allow Poll to consume it first.
	// Now the test: Error path with TaskDelete=ErrRecordNotExist returns nil.
	// Use a fresh task to avoid Poll entanglement.
	task2 := &model.Task{ID: "err-gone-2"}
	s.On("TaskInsert", task2).Return(nil)
	s.On("TaskDelete", "err-gone-2").Return(types.ErrRecordNotExist)

	assert.NoError(t, pq.PushAtOnce(ctx, []*model.Task{task2}))
	_, _ = pq.Poll(ctx, 2, func(*model.Task) (bool, int) { return true, 1 })

	// Mark err-gone-2 as errored; store delete returns "already gone" → tolerated.
	assert.NoError(t, pq.Error(ctx, "err-gone-2", fmt.Errorf("agent crashed")))
}

// TestPersistentQueue_Error_StoreDeleteRealError covers the path where
// TaskDelete returns a non-NotExist error — the persistentQueue must
// surface it.
func TestPersistentQueue_Error_StoreDeleteRealError(t *testing.T) {
	pq, s, ctx, cancel := freshPersistentQueue(t)
	defer cancel()

	task := &model.Task{ID: "err-disk"}
	s.On("TaskInsert", task).Return(nil)
	s.On("TaskDelete", "err-disk").Return(fmt.Errorf("disk full"))

	assert.NoError(t, pq.PushAtOnce(ctx, []*model.Task{task}))
	_, _ = pq.Poll(ctx, 1, func(*model.Task) (bool, int) { return true, 1 })

	err := pq.Error(ctx, "err-disk", fmt.Errorf("agent crashed"))
	assert.Error(t, err, "real store error should propagate")
}

// TestPersistentQueue_ErrorAtOnce_QueueErrorAborts covers the early-return
// when q.Queue.ErrorAtOnce itself fails.
func TestPersistentQueue_ErrorAtOnce_QueueErrorAborts(t *testing.T) {
	pq, s, ctx, cancel := freshPersistentQueue(t)
	defer cancel()

	// Don't push — ErrorAtOnce on unknown IDs returns ErrNotFound from queue.
	err := pq.ErrorAtOnce(ctx, []string{"nope"}, fmt.Errorf("x"))
	assert.Error(t, err)
	s.AssertNotCalled(t, "TaskDelete")
}

// TestPersistentQueue_ErrorAtOnce_StoreDeleteJoinsErrors covers the error-
// joining path when one of N TaskDeletes fails after queue.ErrorAtOnce
// succeeded.
func TestPersistentQueue_ErrorAtOnce_StoreDeleteJoinsErrors(t *testing.T) {
	pq, s, ctx, cancel := freshPersistentQueue(t)
	defer cancel()

	tasks := []*model.Task{{ID: "ea-good"}, {ID: "ea-bad"}}
	for _, task := range tasks {
		s.On("TaskInsert", task).Return(nil)
	}
	s.On("TaskDelete", "ea-good").Return(nil)
	s.On("TaskDelete", "ea-bad").Return(fmt.Errorf("disk full"))

	assert.NoError(t, pq.PushAtOnce(ctx, tasks))

	err := pq.ErrorAtOnce(ctx, []string{"ea-good", "ea-bad"}, fmt.Errorf("batch"))
	assert.Error(t, err, "joined error should surface the failed delete")
	assert.Contains(t, err.Error(), "ea-bad")
}

func TestErrExternal_Error(t *testing.T) {
	wrapped := fmt.Errorf("boom")
	e := NewErrExternal(wrapped)
	assert.Contains(t, e.Error(), "external error")
	assert.Contains(t, e.Error(), "boom")
}

func TestErrExternal_Unwrap(t *testing.T) {
	wrapped := fmt.Errorf("inner")
	e := NewErrExternal(wrapped).(*ErrExternal)
	assert.Equal(t, wrapped, e.Unwrap())
}

func TestErrExternal_NewWithNil(t *testing.T) {
	assert.NoError(t, NewErrExternal(nil), "nil in → nil out")
}

func TestInfoT_String(t *testing.T) {
	info := InfoT{
		Pending:       []*model.Task{{ID: "p1"}},
		Running:       []*model.Task{{ID: "r1"}},
		WaitingOnDeps: []*model.Task{{ID: "w1"}},
	}
	got := info.String()
	assert.Contains(t, got, "p1")
	assert.Contains(t, got, "r1")
	assert.Contains(t, got, "w1")
}

func TestNew_UnsupportedBackend(t *testing.T) {
	_, err := New(context.Background(), Config{Backend: "nonsense"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported queue backend")
}

// TestNew_WithStorePath covers the New() branch that wires a persistent
// queue around the in-memory queue when Config.Store is non-nil.
func TestNew_WithStorePath(t *testing.T) {
	s := storemocks.NewMockStore(t)
	s.On("TaskList").Return([]*model.Task{}, nil)

	q, err := New(context.Background(), Config{Backend: TypeMemory, Store: s})
	assert.NoError(t, err)
	_, ok := q.(*persistentQueue)
	assert.True(t, ok, "queue with Store should be wrapped in persistentQueue")
}

// TestPersistentQueue_Error_QueueErrorPropagates exercises the early-return
// when q.Queue.Error itself fails (task not in queue → fifo's
// removeFromPendingAndWaiting returns ErrNotFound).
func TestPersistentQueue_Error_QueueErrorPropagates(t *testing.T) {
	pq, s, ctx, cancel := freshPersistentQueue(t)
	defer cancel()

	err := pq.Error(ctx, "no-such-task", fmt.Errorf("x"))
	assert.Error(t, err)
	s.AssertNotCalled(t, "TaskDelete")
}

// TestPersistentQueue_PushAtOnce_QueueErrorRollsBackStore covers the
// rollback path: if the in-memory queue.PushAtOnce fails AFTER
// store.TaskInsert succeeded, every inserted task is deleted from the
// store. Uses queueMocks because the real fifo never errors on push.
func TestPersistentQueue_PushAtOnce_QueueErrorRollsBackStore(t *testing.T) {
	mockStore := storemocks.NewMockStore(t)
	mockQueue := newFailingPushQueue()

	pq := &persistentQueue{Queue: mockQueue, store: mockStore}

	tasks := []*model.Task{{ID: "rb1"}, {ID: "rb2"}}
	for _, task := range tasks {
		mockStore.On("TaskInsert", task).Return(nil)
		mockStore.On("TaskDelete", task.ID).Return(nil)
	}

	err := pq.PushAtOnce(context.Background(), tasks)
	assert.Error(t, err, "queue PushAtOnce error should propagate")
	mockStore.AssertCalled(t, "TaskDelete", "rb1")
	mockStore.AssertCalled(t, "TaskDelete", "rb2")
}

// TestPersistentQueue_PushAtOnce_RollbackDeleteAlsoFails covers the inner
// error-on-rollback path: TaskInsert succeeded, queue PushAtOnce failed,
// TaskDelete during rollback also fails — the rollback delete error is
// returned (overriding the original queue error).
func TestPersistentQueue_PushAtOnce_RollbackDeleteAlsoFails(t *testing.T) {
	mockStore := storemocks.NewMockStore(t)
	mockQueue := newFailingPushQueue()
	pq := &persistentQueue{Queue: mockQueue, store: mockStore}

	task := &model.Task{ID: "rb-dead"}
	mockStore.On("TaskInsert", task).Return(nil)
	mockStore.On("TaskDelete", "rb-dead").Return(fmt.Errorf("disk full"))

	err := pq.PushAtOnce(context.Background(), []*model.Task{task})
	assert.Error(t, err)
}

// TestWithTaskStore_ZombieDeleteErrorIsLogged covers the log-and-continue
// branch when TaskDelete fails during zombie eviction.
func TestWithTaskStore_ZombieDeleteErrorIsLogged(t *testing.T) {
	s := storemocks.NewMockStore(t)
	s.On("TaskList").Return([]*model.Task{
		{ID: "zombie", PipelineID: 1},
	}, nil)
	s.On("GetPipeline", int64(1)).Return(&model.Pipeline{ID: 1, Status: model.StatusKilled}, nil)
	s.On("TaskDelete", "zombie").Return(fmt.Errorf("disk full"))

	ctx := context.Background()
	inner, _ := New(ctx, Config{Backend: TypeMemory})

	// Should not panic even though TaskDelete errors.
	WithTaskStore(ctx, inner, s)

	s.AssertCalled(t, "TaskDelete", "zombie")
}

// failingPushQueue is a Queue that always returns an error from PushAtOnce.
// Used to exercise the persistentQueue rollback path that the real fifo
// can't trigger (fifo.PushAtOnce never errors).
type failingPushQueue struct{}

func newFailingPushQueue() *failingPushQueue { return &failingPushQueue{} }

func (q *failingPushQueue) PushAtOnce(context.Context, []*model.Task) error {
	return fmt.Errorf("simulated queue push failure")
}
func (q *failingPushQueue) Poll(context.Context, int64, FilterFn) (*model.Task, error) {
	return nil, nil
}
func (q *failingPushQueue) Extend(context.Context, int64, string) error           { return nil }
func (q *failingPushQueue) Done(context.Context, string, model.StatusValue) error { return nil }
func (q *failingPushQueue) Error(context.Context, string, error) error            { return nil }
func (q *failingPushQueue) ErrorAtOnce(context.Context, []string, error) error    { return nil }
func (q *failingPushQueue) Wait(context.Context, string) error                    { return nil }
func (q *failingPushQueue) Info(context.Context) InfoT                            { return InfoT{} }
func (q *failingPushQueue) Pause()                                                {}
func (q *failingPushQueue) Resume()                                               {}
func (q *failingPushQueue) KickAgentWorkers(int64)                                {}
func (q *failingPushQueue) SetDispatchHook(DispatchFunc)                          {}
func (q *failingPushQueue) UpdatePriority(string, int64) (int64, error) {
	return 0, ErrNotFound
}
