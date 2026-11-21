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
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/store"
	"go.woodpecker-ci.org/woodpecker/v3/server/store/types"
)

// #400: a server restart must not forget the work agents are running.
//
// These tests drive the REAL fifo through the REAL persistent wrapper twice,
// over one store that keeps a tasks table, the way a restart does: queue A
// takes the work, queue B is built from what A left in the store. Only the
// store is a fake, and it is a table, not a script of expected calls, so what
// queue B restores is whatever queue A actually persisted.

type tableStore struct {
	store.Store // unimplemented methods panic: the restore must not need them
	mu          sync.Mutex
	tasks       map[string]*model.Task
	workflows   map[int64]*model.Workflow
	pipelines   map[int64]*model.Pipeline
}

func newTableStore() *tableStore {
	return &tableStore{tasks: map[string]*model.Task{}, workflows: map[int64]*model.Workflow{}, pipelines: map[int64]*model.Pipeline{}}
}

func (s *tableStore) TaskList() ([]*model.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*model.Task, 0, len(s.tasks))
	for _, t := range s.tasks {
		out = append(out, rowCopy(t))
	}
	return out, nil
}

func (s *tableStore) TaskInsert(t *model.Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.tasks[t.ID]; dup {
		return types.ErrInsertDuplicateDetected
	}
	s.tasks[t.ID] = rowCopy(t)
	return nil
}

// rowCopy stands in for the database round trip: a stored row shares no maps
// with the in-memory task it was written from.
func rowCopy(t *model.Task) *model.Task {
	b, err := json.Marshal(t)
	if err != nil {
		panic(err)
	}
	var c model.Task
	if err := json.Unmarshal(b, &c); err != nil {
		panic(err)
	}
	return &c
}

func (s *tableStore) TaskDelete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tasks[id]; !ok {
		return types.ErrRecordNotExist
	}
	delete(s.tasks, id)
	return nil
}

func (s *tableStore) GetPipeline(id int64) (*model.Pipeline, error) {
	if p, ok := s.pipelines[id]; ok {
		return p, nil
	}
	return nil, types.ErrRecordNotExist
}

func (s *tableStore) WorkflowLoad(id int64) (*model.Workflow, error) {
	if w, ok := s.workflows[id]; ok {
		return w, nil
	}
	return nil, types.ErrRecordNotExist
}

func (s *tableStore) hasTask(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.tasks[id]
	return ok
}

func anyTask(*model.Task) (bool, int) { return true, 1 }

func pollWithin(q Queue, agentID int64, d time.Duration) *model.Task {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	task, _ := q.Poll(ctx, agentID, anyTask)
	return task
}

func runningIDs(q Queue) map[string]int64 {
	out := map[string]int64{}
	for _, t := range q.Info(context.Background()).Running {
		out[t.ID] = t.AgentID
	}
	return out
}

// restartWith builds the queue a restarted server builds: a fresh fifo restored
// from the store. extension shortens the lease when a test needs to see expiry.
func restartWith(t *testing.T, s store.Store, extension time.Duration) Queue {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	inner := NewMemoryQueue(ctx)
	if extension > 0 {
		inner.(*fifo).extension = extension
	}
	return WithTaskStore(ctx, inner, s)
}

// buildThenDeploy is a two-workflow pipeline: "100" runs on agent 7 when the
// server restarts, and "101" depends on it.
func buildThenDeploy(t *testing.T) *tableStore {
	t.Helper()
	s := newTableStore()
	s.pipelines[9] = &model.Pipeline{ID: 9, Status: model.StatusRunning}
	s.workflows[100] = &model.Workflow{ID: 100, PipelineID: 9, State: model.StatusPending}
	s.workflows[101] = &model.Workflow{ID: 101, PipelineID: 9, State: model.StatusPending}

	before := restartWith(t, s, 0)
	require.NoError(t, before.PushAtOnce(context.Background(), []*model.Task{
		{ID: "100", PipelineID: 9},
		{ID: "101", PipelineID: 9, Dependencies: []string{"100"}, DepStatus: map[string]model.StatusValue{}},
	}))
	got := pollWithin(before, 7, 2*time.Second)
	require.NotNil(t, got)
	require.Equal(t, "100", got.ID)
	// rpc.Init: the agent started it.
	s.workflows[100].State = model.StatusRunning
	s.workflows[100].AgentID = 7
	return s
}

// The work an agent is running survives a restart: it is back in q.running,
// on the same agent, so the reconcilers see it, the agent's lease renewal is
// accepted, and its dependents keep waiting for it.
func TestWithTaskStore_RestoresRunningTaskOnItsAgent(t *testing.T) {
	s := buildThenDeploy(t)

	after := restartWith(t, s, 0)

	assert.Equal(t, map[string]int64{"100": 7}, runningIDs(after),
		"a task an agent was running must be restored as running on that agent, not forgotten")
	running := after.Info(context.Background()).Running
	require.Len(t, running, 1)
	assert.Equal(t, int64(9), running[0].PipelineID,
		"the restored task must name its pipeline: that is what the startup reconcile reads to spare it")
	assert.NoError(t, after.Extend(context.Background(), 7, "100"),
		"the running agent's lease renewal must be accepted after the restart")
	assert.Nil(t, pollWithin(after, 8, 300*time.Millisecond),
		"a dependent must not be released while the work it depends on is still running")

	require.NoError(t, after.Done(context.Background(), "100", model.StatusSuccess))
	next := pollWithin(after, 8, 2*time.Second)
	require.NotNil(t, next, "the dependent runs once its dependency is done")
	assert.Equal(t, "101", next.ID)
	assert.Equal(t, model.StatusSuccess, next.DepStatus["100"])
}

// A restored task whose agent never renews it is not held forever, and it is
// not silently run again either. Everything restored as running has started,
// so its expiry is handed to the release path, which kills work that ran and
// re-queues work that did not (#72), the same as for a disconnected agent. A
// live agent is not proof the workflow is live (an agent restarted with the
// server keeps its id but not its work), so agent liveness is not consulted.
func TestWithTaskStore_RestoredTaskWithoutRenewalIsReleasedNotRerun(t *testing.T) {
	s := buildThenDeploy(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	inner := NewMemoryQueue(ctx)
	inner.(*fifo).extension = 50 * time.Millisecond
	released := make(chan []*model.Task, 4)
	inner.SetRestoredExpiryFn(func(tasks []*model.Task) { released <- tasks })
	after := WithTaskStore(ctx, inner, s)

	select {
	case got := <-released:
		require.Len(t, got, 1)
		assert.Equal(t, "100", got[0].ID)
		assert.Equal(t, int64(7), got[0].AgentID, "release acts for the agent that held it")
	case <-time.After(3 * time.Second):
		t.Fatal("an unrenewed restored lease must be released in bounded time")
	}
	for _, p := range after.Info(context.Background()).Pending {
		assert.NotEqual(t, "100", p.ID, "started work must not be put back to run again on expiry")
	}
}

// A task the agent renews is not released.
func TestWithTaskStore_RenewedRestoredTaskIsKept(t *testing.T) {
	s := buildThenDeploy(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	inner := NewMemoryQueue(ctx)
	inner.(*fifo).extension = 300 * time.Millisecond
	released := make(chan []*model.Task, 4)
	inner.SetRestoredExpiryFn(func(tasks []*model.Task) { released <- tasks })
	after := WithTaskStore(ctx, inner, s)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		require.NoError(t, after.Extend(context.Background(), 7, "100"))
		select {
		case <-released:
			t.Fatal("a renewed restored task must not be released")
		case <-time.After(100 * time.Millisecond):
		}
	}
	assert.Equal(t, map[string]int64{"100": 7}, runningIDs(after))
}

// The store row now lives until the work is finished, so a second restart
// still finds it, and Done removes it.
func TestPersistentQueue_RowLivesUntilDone(t *testing.T) {
	s := buildThenDeploy(t)
	assert.True(t, s.hasTask("100"), "a polled task must stay in the store while it runs")

	after := restartWith(t, s, 0)
	require.NoError(t, after.Done(context.Background(), "100", model.StatusSuccess))
	assert.False(t, s.hasTask("100"), "Done must remove the finished task from the store")
}

// A task whose workflow already finished (its Done was lost in the restart)
// is evicted, not run again.
func TestWithTaskStore_EvictsTaskOfFinishedWorkflow(t *testing.T) {
	s := buildThenDeploy(t)
	s.workflows[100].State = model.StatusSuccess

	after := restartWith(t, s, 0)

	assert.Empty(t, runningIDs(after))
	assert.False(t, s.hasTask("100"), "a finished workflow's task must be evicted")
	next := pollWithin(after, 8, 2*time.Second)
	require.NotNil(t, next)
	assert.Equal(t, "101", next.ID, "only the dependent remains to run")
	assert.Equal(t, model.StatusSuccess, next.DepStatus["100"],
		"the dependent must see the finished workflow's outcome, not an empty status")
}

// planRestore's edge rows: a task whose pipeline is gone is evicted; a task
// whose workflow cannot be read waits, which is what every stored row meant
// before #400; a dependent with no status map still learns its dependency's
// outcome.
func TestPlanRestore_EdgeRows(t *testing.T) {
	s := newTableStore()
	s.pipelines[9] = &model.Pipeline{ID: 9, Status: model.StatusRunning}
	s.workflows[100] = &model.Workflow{ID: 100, PipelineID: 9, State: model.StatusFailure}

	plan := planRestore(s, []*model.Task{
		{ID: "100", PipelineID: 9},
		{ID: "101", PipelineID: 9, Dependencies: []string{"100"}},
		{ID: "not-a-workflow-id", PipelineID: 9},
		{ID: "500", PipelineID: 404},
	})

	assert.ElementsMatch(t, []string{"100", "500"}, plan.evict)
	assert.Empty(t, plan.running)
	ids := map[string]*model.Task{}
	for _, task := range plan.pending {
		ids[task.ID] = task
	}
	require.Contains(t, ids, "101", "a task whose workflow row cannot be read is restored as waiting")
	require.Contains(t, ids, "not-a-workflow-id")
	assert.Equal(t, model.StatusFailure, ids["101"].DepStatus["100"])
}
