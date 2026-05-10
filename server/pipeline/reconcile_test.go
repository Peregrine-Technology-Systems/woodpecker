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
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"go.woodpecker-ci.org/woodpecker/v3/server"
	forge_mocks "go.woodpecker-ci.org/woodpecker/v3/server/forge/mocks"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/queue"
	queue_mocks "go.woodpecker-ci.org/woodpecker/v3/server/queue/mocks"
	services_mocks "go.woodpecker-ci.org/woodpecker/v3/server/services/mocks"
	"go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
)

// queueWithTasks returns a mock queue.Queue whose Info() reports the given
// pipeline IDs as having active tasks (split arbitrarily across Pending /
// Running / WaitingOnDeps doesn't matter — activePipelineSet treats all
// three identically).
func queueWithTasks(t *testing.T, pipelineIDs ...int64) *queue_mocks.MockQueue {
	t.Helper()
	q := queue_mocks.NewMockQueue(t)
	pending := make([]*model.Task, 0, len(pipelineIDs))
	for _, id := range pipelineIDs {
		pending = append(pending, &model.Task{ID: "t", PipelineID: id})
	}
	q.On("Info", mock.Anything).Return(queue.InfoT{
		Pending: pending,
	})
	return q
}

func TestReconcileOrphanedAtStartup_KillsRunningPipelines(t *testing.T) {
	mockStore := mocks.NewMockStore(t)
	q := queueWithTasks(t) // empty queue → all running pipelines orphaned

	repos := []*model.Repo{{ID: 1, FullName: "org/scanner"}}
	mockStore.On("RepoListAll", true, mock.Anything).Return(repos, nil)

	pipelines := []*model.Pipeline{
		{ID: 100, Number: 1020, Status: model.StatusRunning, RepoID: 1},
	}
	mockStore.On("GetActivePipelineList", repos[0]).Return(pipelines, nil)

	mockStore.On("UpdatePipeline", mock.MatchedBy(func(p *model.Pipeline) bool {
		return p.ID == 100 && p.Status == model.StatusKilled
	})).Return(nil)

	workflows := []*model.Workflow{{ID: 50, State: model.StatusRunning, Name: "ci"}}
	mockStore.On("WorkflowGetTree", pipelines[0]).Return(workflows, nil)
	mockStore.On("WorkflowUpdate", mock.MatchedBy(func(w *model.Workflow) bool {
		return w.ID == 50 && w.State == model.StatusKilled
	})).Return(nil)

	count := ReconcileOrphanedAtStartup(mockStore, q)
	assert.Equal(t, 1, count)
}

func TestReconcileOrphanedAtStartup_SkipsPendingPipelines(t *testing.T) {
	mockStore := mocks.NewMockStore(t)
	q := queueWithTasks(t)

	repos := []*model.Repo{{ID: 1, FullName: "org/repo"}}
	mockStore.On("RepoListAll", true, mock.Anything).Return(repos, nil)
	mockStore.On("GetActivePipelineList", repos[0]).Return([]*model.Pipeline{
		{ID: 200, Number: 42, Status: model.StatusPending, RepoID: 1},
	}, nil)

	count := ReconcileOrphanedAtStartup(mockStore, q)
	assert.Equal(t, 0, count)
	mockStore.AssertNotCalled(t, "UpdatePipeline", mock.Anything)
}

func TestReconcileOrphanedAtStartup_NoActiveRepos(t *testing.T) {
	mockStore := mocks.NewMockStore(t)
	q := queueWithTasks(t)
	mockStore.On("RepoListAll", true, mock.Anything).Return([]*model.Repo{}, nil)

	count := ReconcileOrphanedAtStartup(mockStore, q)
	assert.Equal(t, 0, count)
}

// TestReconcileOrphanedAtStartup_SkipsPipelineWithActiveTask is the #188
// contract: even at startup, if the persistent queue had tasks for this
// pipeline (restored via WithTaskStore), the reconciler must NOT kill it.
func TestReconcileOrphanedAtStartup_SkipsPipelineWithActiveTask(t *testing.T) {
	mockStore := mocks.NewMockStore(t)
	q := queueWithTasks(t, 100) // pipeline 100 has a task in the queue

	repos := []*model.Repo{{ID: 1, FullName: "org/r"}}
	mockStore.On("RepoListAll", true, mock.Anything).Return(repos, nil)
	mockStore.On("GetActivePipelineList", repos[0]).Return([]*model.Pipeline{
		{ID: 100, Number: 1, Status: model.StatusRunning, RepoID: 1},
	}, nil)

	count := ReconcileOrphanedAtStartup(mockStore, q)
	assert.Equal(t, 0, count, "pipeline with active queue task must not be killed")
	mockStore.AssertNotCalled(t, "UpdatePipeline")
}

// TestReconcileOrphanedAtStartup_QueueNilFallsBackToAggressive locks in
// the back-compat alias path: when the queue is nil (legacy callers using
// ReconcileOrphaned), every running pipeline is treated as orphaned.
func TestReconcileOrphanedAtStartup_QueueNilFallsBackToAggressive(t *testing.T) {
	mockStore := mocks.NewMockStore(t)

	repos := []*model.Repo{{ID: 1, FullName: "org/r"}}
	mockStore.On("RepoListAll", true, mock.Anything).Return(repos, nil)
	mockStore.On("GetActivePipelineList", repos[0]).Return([]*model.Pipeline{
		{ID: 100, Number: 1, Status: model.StatusRunning, RepoID: 1},
	}, nil)
	mockStore.On("UpdatePipeline", mock.Anything).Return(nil)
	mockStore.On("WorkflowGetTree", mock.Anything).Return([]*model.Workflow{}, nil)

	count := ReconcileOrphanedAtStartup(mockStore, nil)
	assert.Equal(t, 1, count, "nil queue → empty active set → kill all running")
}

// TestOrphanReconcilerTick_SkipsPipelineWithActiveTask is the steady-state
// guard: a long-running pts-build pipeline with the agent heartbeating
// (task in q.Running because Extend is called) must NEVER get killed by
// the timer.
func TestOrphanReconcilerTick_SkipsPipelineWithActiveTask(t *testing.T) {
	mockStore := mocks.NewMockStore(t)
	q := queueWithTasks(t, 700) // pts-build pipeline has active task

	repos := []*model.Repo{{ID: 13, FullName: "Peregrine-Technology-Systems/woodpecker"}}
	mockStore.On("RepoListAll", true, mock.Anything).Return(repos, nil)
	mockStore.On("GetActivePipelineList", repos[0]).Return([]*model.Pipeline{
		{ID: 700, Number: 367, Status: model.StatusRunning, RepoID: 13},
	}, nil)

	r := NewOrphanReconciler(q)
	count := r.Tick(mockStore)
	assert.Equal(t, 0, count, "actively-running pipeline must not be killed")
	mockStore.AssertNotCalled(t, "UpdatePipeline")
}

// TestOrphanReconcilerTick_KillsPipelineWithoutQueueTask covers the actual
// orphan path: pipeline DB-running, queue has no task for it, agent
// disappeared without replacing the task → kill.
func TestOrphanReconcilerTick_KillsPipelineWithoutQueueTask(t *testing.T) {
	mockStore := mocks.NewMockStore(t)
	q := queueWithTasks(t, 999) // some other pipeline, NOT 100

	repos := []*model.Repo{{ID: 1, FullName: "org/r"}}
	mockStore.On("RepoListAll", true, mock.Anything).Return(repos, nil)
	mockStore.On("GetActivePipelineList", repos[0]).Return([]*model.Pipeline{
		{ID: 100, Number: 1, Status: model.StatusRunning, RepoID: 1},
	}, nil)
	mockStore.On("UpdatePipeline", mock.Anything).Return(nil)
	mockStore.On("WorkflowGetTree", mock.Anything).Return([]*model.Workflow{}, nil)

	r := NewOrphanReconciler(q)
	count := r.Tick(mockStore)
	assert.Equal(t, 1, count, "pipeline with no queue task should be killed")
}

// TestOrphanReconcilerTick_RepoListErrorReturnsZero covers defensive paths.
func TestOrphanReconcilerTick_RepoListErrorReturnsZero(t *testing.T) {
	mockStore := mocks.NewMockStore(t)
	q := queueWithTasks(t)
	mockStore.On("RepoListAll", true, mock.Anything).Return(nil, assert.AnError)

	r := NewOrphanReconciler(q)
	assert.Equal(t, 0, r.Tick(mockStore))
}

// TestOrphanReconcilerTick_GetActiveErrorContinues — error on one repo
// doesn't abort the pass.
func TestOrphanReconcilerTick_GetActiveErrorContinues(t *testing.T) {
	mockStore := mocks.NewMockStore(t)
	q := queueWithTasks(t)
	repos := []*model.Repo{{ID: 1, FullName: "org/broken"}}
	mockStore.On("RepoListAll", true, mock.Anything).Return(repos, nil)
	mockStore.On("GetActivePipelineList", repos[0]).Return(nil, assert.AnError)

	r := NewOrphanReconciler(q)
	assert.Equal(t, 0, r.Tick(mockStore))
}

// TestReconcileOrphanedAtStartup_RepoListErrorReturnsZero — same defensive
// path on the startup variant.
func TestReconcileOrphanedAtStartup_RepoListErrorReturnsZero(t *testing.T) {
	mockStore := mocks.NewMockStore(t)
	q := queueWithTasks(t)
	mockStore.On("RepoListAll", true, mock.Anything).Return(nil, assert.AnError)

	assert.Equal(t, 0, ReconcileOrphanedAtStartup(mockStore, q))
}

// TestReconcileOrphanedAtStartup_GetActiveErrorContinues — startup path's
// per-repo continue branch.
func TestReconcileOrphanedAtStartup_GetActiveErrorContinues(t *testing.T) {
	mockStore := mocks.NewMockStore(t)
	q := queueWithTasks(t)
	repos := []*model.Repo{{ID: 1, FullName: "org/broken"}, {ID: 2, FullName: "org/works"}}
	mockStore.On("RepoListAll", true, mock.Anything).Return(repos, nil)
	mockStore.On("GetActivePipelineList", repos[0]).Return(nil, assert.AnError)
	mockStore.On("GetActivePipelineList", repos[1]).Return([]*model.Pipeline{}, nil)

	assert.Equal(t, 0, ReconcileOrphanedAtStartup(mockStore, q))
}

func TestReconcileOrphaned_BackCompatAlias(t *testing.T) {
	mockStore := mocks.NewMockStore(t)
	mockStore.On("RepoListAll", true, mock.Anything).Return([]*model.Repo{}, nil)
	assert.Equal(t, 0, ReconcileOrphaned(mockStore))
}

// TestKillOrphan_UpdatePipelineFailureReturnsFalse — pipeline-update
// failure aborts the kill; no workflow walk attempted.
func TestKillOrphan_UpdatePipelineFailureReturnsFalse(t *testing.T) {
	mockStore := mocks.NewMockStore(t)
	q := queueWithTasks(t)
	repos := []*model.Repo{{ID: 1, FullName: "org/r"}}
	mockStore.On("RepoListAll", true, mock.Anything).Return(repos, nil)
	mockStore.On("GetActivePipelineList", repos[0]).Return([]*model.Pipeline{
		{ID: 50, Number: 1, Status: model.StatusRunning, RepoID: 1},
	}, nil)
	mockStore.On("UpdatePipeline", mock.Anything).Return(assert.AnError)

	count := ReconcileOrphanedAtStartup(mockStore, q)
	assert.Equal(t, 0, count)
	mockStore.AssertNotCalled(t, "WorkflowGetTree")
}

// TestKillOrphan_WorkflowGetTreeFailureCountsAsKilled — pipeline kill
// remains durable even if we can't load its workflows.
func TestKillOrphan_WorkflowGetTreeFailureCountsAsKilled(t *testing.T) {
	mockStore := mocks.NewMockStore(t)
	q := queueWithTasks(t)
	repos := []*model.Repo{{ID: 1, FullName: "org/r"}}
	mockStore.On("RepoListAll", true, mock.Anything).Return(repos, nil)
	mockStore.On("GetActivePipelineList", repos[0]).Return([]*model.Pipeline{
		{ID: 51, Number: 1, Status: model.StatusRunning, RepoID: 1},
	}, nil)
	mockStore.On("UpdatePipeline", mock.Anything).Return(nil)
	mockStore.On("WorkflowGetTree", mock.Anything).Return(nil, assert.AnError)

	count := ReconcileOrphanedAtStartup(mockStore, q)
	assert.Equal(t, 1, count)
}

// TestKillOrphan_WorkflowUpdateFailureLoggedButContinues — per-workflow
// update failures are logged but don't block other workflows or the kill.
func TestKillOrphan_WorkflowUpdateFailureLoggedButContinues(t *testing.T) {
	mockStore := mocks.NewMockStore(t)
	q := queueWithTasks(t)
	repos := []*model.Repo{{ID: 1, FullName: "org/r"}}
	mockStore.On("RepoListAll", true, mock.Anything).Return(repos, nil)
	mockStore.On("GetActivePipelineList", repos[0]).Return([]*model.Pipeline{
		{ID: 52, Number: 1, Status: model.StatusRunning, RepoID: 1},
	}, nil)
	mockStore.On("UpdatePipeline", mock.Anything).Return(nil)
	mockStore.On("WorkflowGetTree", mock.Anything).Return([]*model.Workflow{
		{ID: 1, State: model.StatusRunning},
		{ID: 2, State: model.StatusRunning},
	}, nil)
	mockStore.On("WorkflowUpdate", mock.MatchedBy(func(w *model.Workflow) bool { return w.ID == 1 })).Return(assert.AnError)
	mockStore.On("WorkflowUpdate", mock.MatchedBy(func(w *model.Workflow) bool { return w.ID == 2 })).Return(nil)

	count := ReconcileOrphanedAtStartup(mockStore, q)
	assert.Equal(t, 1, count)
}

// TestKillOrphan_PostsForgeStatusWhenManagerWired locks in the #181 fix:
// when a kill happens, the forge gets a final status post.
func TestKillOrphan_PostsForgeStatusWhenManagerWired(t *testing.T) {
	prevManager := server.Config.Services.Manager
	defer func() { server.Config.Services.Manager = prevManager }()

	mockStore := mocks.NewMockStore(t)
	mockForge := forge_mocks.NewMockForge(t)
	mockManager := services_mocks.NewMockManager(t)
	server.Config.Services.Manager = mockManager
	q := queueWithTasks(t)

	repos := []*model.Repo{{ID: 1, FullName: "org/r", UserID: 42}}
	mockStore.On("RepoListAll", true, mock.Anything).Return(repos, nil)
	mockStore.On("GetActivePipelineList", repos[0]).Return([]*model.Pipeline{
		{ID: 100, Number: 1, Status: model.StatusRunning, RepoID: 1},
	}, nil)
	mockStore.On("UpdatePipeline", mock.Anything).Return(nil)
	mockStore.On("WorkflowGetTree", mock.Anything).Return([]*model.Workflow{
		{ID: 50, State: model.StatusRunning, Name: "ci"},
	}, nil)
	mockStore.On("WorkflowUpdate", mock.Anything).Return(nil)
	mockStore.On("GetUser", int64(42)).Return(&model.User{ID: 42, Login: "u"}, nil)
	mockManager.On("ForgeFromRepo", repos[0]).Return(mockForge, nil)
	mockForge.On("Refresh", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	mockForge.On("Status", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	count := ReconcileOrphanedAtStartup(mockStore, q)
	assert.Equal(t, 1, count)
	mockForge.AssertCalled(t, "Status", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestKillOrphan_NoForgeStatusWhenManagerMissing(t *testing.T) {
	prevManager := server.Config.Services.Manager
	defer func() { server.Config.Services.Manager = prevManager }()
	server.Config.Services.Manager = nil

	mockStore := mocks.NewMockStore(t)
	q := queueWithTasks(t)
	repos := []*model.Repo{{ID: 1, FullName: "org/r", UserID: 42}}
	mockStore.On("RepoListAll", true, mock.Anything).Return(repos, nil)
	mockStore.On("GetActivePipelineList", repos[0]).Return([]*model.Pipeline{
		{ID: 100, Number: 1, Status: model.StatusRunning, RepoID: 1},
	}, nil)
	mockStore.On("UpdatePipeline", mock.Anything).Return(nil)
	mockStore.On("WorkflowGetTree", mock.Anything).Return([]*model.Workflow{}, nil)

	count := ReconcileOrphanedAtStartup(mockStore, q)
	assert.Equal(t, 1, count)
	mockStore.AssertNotCalled(t, "GetUser")
}

func TestKillOrphan_ForgeLookupFailureLogged(t *testing.T) {
	prevManager := server.Config.Services.Manager
	defer func() { server.Config.Services.Manager = prevManager }()

	mockStore := mocks.NewMockStore(t)
	mockManager := services_mocks.NewMockManager(t)
	server.Config.Services.Manager = mockManager
	q := queueWithTasks(t)

	repos := []*model.Repo{{ID: 1, FullName: "org/r", UserID: 42}}
	mockStore.On("RepoListAll", true, mock.Anything).Return(repos, nil)
	mockStore.On("GetActivePipelineList", repos[0]).Return([]*model.Pipeline{
		{ID: 100, Number: 1, Status: model.StatusRunning, RepoID: 1},
	}, nil)
	mockStore.On("UpdatePipeline", mock.Anything).Return(nil)
	mockStore.On("WorkflowGetTree", mock.Anything).Return([]*model.Workflow{}, nil)
	mockStore.On("GetUser", int64(42)).Return(&model.User{ID: 42}, nil)
	mockManager.On("ForgeFromRepo", repos[0]).Return(nil, assert.AnError)

	assert.Equal(t, 1, ReconcileOrphanedAtStartup(mockStore, q))
}

func TestKillOrphan_GetUserFailureLogged(t *testing.T) {
	prevManager := server.Config.Services.Manager
	defer func() { server.Config.Services.Manager = prevManager }()

	mockStore := mocks.NewMockStore(t)
	mockManager := services_mocks.NewMockManager(t)
	server.Config.Services.Manager = mockManager
	q := queueWithTasks(t)

	repos := []*model.Repo{{ID: 1, FullName: "org/r", UserID: 42}}
	mockStore.On("RepoListAll", true, mock.Anything).Return(repos, nil)
	mockStore.On("GetActivePipelineList", repos[0]).Return([]*model.Pipeline{
		{ID: 100, Number: 1, Status: model.StatusRunning, RepoID: 1},
	}, nil)
	mockStore.On("UpdatePipeline", mock.Anything).Return(nil)
	mockStore.On("WorkflowGetTree", mock.Anything).Return([]*model.Workflow{}, nil)
	mockStore.On("GetUser", int64(42)).Return(nil, assert.AnError)

	assert.Equal(t, 1, ReconcileOrphanedAtStartup(mockStore, q))
}

// TestActivePipelineSet_NilQueueReturnsEmpty — fail-open: nil queue means
// no information; behave as if no pipelines are active.
func TestActivePipelineSet_NilQueueReturnsEmpty(t *testing.T) {
	got := activePipelineSet(nil)
	assert.Empty(t, got)
}

// TestKillOrphan_AuditLog covers the #193 audit fields on the reconcile
// kill path: every reconciler-driven kill emits the same structured log
// shape Cancel() does, with reason=reconcile_orphaned.
func TestKillOrphan_AuditLog(t *testing.T) {
	buf := &bytes.Buffer{}
	prev := log.Logger
	log.Logger = zerolog.New(buf).With().Logger()
	defer func() { log.Logger = prev }()

	mockStore := mocks.NewMockStore(t)
	q := queueWithTasks(t) // empty queue → orphan
	repos := []*model.Repo{{ID: 1, FullName: "org/audit"}}
	mockStore.On("RepoListAll", true, mock.Anything).Return(repos, nil)
	mockStore.On("GetActivePipelineList", repos[0]).Return([]*model.Pipeline{
		{ID: 7777, Number: 5, Status: model.StatusRunning, Started: 9999, RepoID: 1},
	}, nil)
	mockStore.On("UpdatePipeline", mock.Anything).Return(nil)
	mockStore.On("WorkflowGetTree", mock.Anything).Return([]*model.Workflow{}, nil)

	count := ReconcileOrphanedAtStartup(mockStore, q)
	assert.Equal(t, 1, count)

	var rec map[string]interface{}
	for _, line := range strings.Split(buf.String(), "\n") {
		if !strings.Contains(line, "pipeline cancel: transitioning") {
			continue
		}
		assert.NoError(t, json.Unmarshal([]byte(line), &rec))
		break
	}
	if assert.NotNil(t, rec, "audit log expected: %q", buf.String()) {
		assert.Equal(t, float64(7777), rec["pipeline_id"])
		assert.Equal(t, "org/audit", rec["repo"])
		assert.Equal(t, "running", rec["prior_status"])
		assert.Equal(t, "killed", rec["new_status"])
		assert.Equal(t, false, rec["never_dispatched"])
		assert.Equal(t, "reconcile_orphaned", rec["reason"])
	}
}

// TestActivePipelineSet_PullsFromAllThreeListsAndDedupes verifies the set
// covers Pending, Running, AND WaitingOnDeps (deps clearing later moves
// them to pending; ignoring this list would cause a brief misfire).
func TestActivePipelineSet_PullsFromAllThreeListsAndDedupes(t *testing.T) {
	q := queue_mocks.NewMockQueue(t)
	q.On("Info", mock.Anything).Return(queue.InfoT{
		Pending:       []*model.Task{{PipelineID: 1}, {PipelineID: 2}},
		Running:       []*model.Task{{PipelineID: 3}, {PipelineID: 1}}, // dedupe 1
		WaitingOnDeps: []*model.Task{{PipelineID: 4}},
	})

	got := activePipelineSet(q)
	assert.Len(t, got, 4)
	for _, id := range []int64{1, 2, 3, 4} {
		_, ok := got[id]
		assert.True(t, ok, "expected pipeline %d in active set", id)
	}
}
