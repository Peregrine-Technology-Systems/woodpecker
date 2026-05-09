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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"go.woodpecker-ci.org/woodpecker/v3/server"
	forge_mocks "go.woodpecker-ci.org/woodpecker/v3/server/forge/mocks"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	services_mocks "go.woodpecker-ci.org/woodpecker/v3/server/services/mocks"
	"go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
)

func TestReconcileOrphanedAtStartup_KillsRunningPipelines(t *testing.T) {
	mockStore := mocks.NewMockStore(t)

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

	count := ReconcileOrphanedAtStartup(mockStore)
	assert.Equal(t, 1, count)
	mockStore.AssertCalled(t, "UpdatePipeline", mock.Anything)
	mockStore.AssertCalled(t, "WorkflowUpdate", mock.Anything)
}

func TestReconcileOrphanedAtStartup_SkipsPendingPipelines(t *testing.T) {
	mockStore := mocks.NewMockStore(t)

	repos := []*model.Repo{{ID: 1, FullName: "org/repo"}}
	mockStore.On("RepoListAll", true, mock.Anything).Return(repos, nil)

	pipelines := []*model.Pipeline{
		{ID: 200, Number: 42, Status: model.StatusPending, RepoID: 1},
	}
	mockStore.On("GetActivePipelineList", repos[0]).Return(pipelines, nil)

	count := ReconcileOrphanedAtStartup(mockStore)
	assert.Equal(t, 0, count)
	mockStore.AssertNotCalled(t, "UpdatePipeline", mock.Anything)
}

func TestReconcileOrphanedAtStartup_NoActiveRepos(t *testing.T) {
	mockStore := mocks.NewMockStore(t)
	mockStore.On("RepoListAll", true, mock.Anything).Return([]*model.Repo{}, nil)

	count := ReconcileOrphanedAtStartup(mockStore)
	assert.Equal(t, 0, count)
}

// TestOrphanReconciler_FirstTickGraceHold encodes the #176 fix: the first
// time the timer observes a "running" pipeline with no queue task it must NOT
// kill — the agent may simply have momentarily disconnected.
func TestOrphanReconciler_FirstTickGraceHold(t *testing.T) {
	mockStore := mocks.NewMockStore(t)

	repos := []*model.Repo{{ID: 1, FullName: "org/emailer"}}
	mockStore.On("RepoListAll", true, mock.Anything).Return(repos, nil)

	pipelines := []*model.Pipeline{
		{ID: 699, Number: 699, Status: model.StatusRunning, RepoID: 1},
	}
	mockStore.On("GetActivePipelineList", repos[0]).Return(pipelines, nil)

	r := NewOrphanReconciler()
	count := r.Tick(mockStore)

	assert.Equal(t, 0, count, "first tick must not kill")
	mockStore.AssertNotCalled(t, "UpdatePipeline", mock.Anything)
	mockStore.AssertNotCalled(t, "WorkflowUpdate", mock.Anything)
	assert.Equal(t, 1, r.PendingCount(), "pipeline must be tracked in firstSeen")
}

// TestOrphanReconciler_SecondTickKills verifies that a pipeline observed
// orphaned on two consecutive ticks is killed on the second.
func TestOrphanReconciler_SecondTickKills(t *testing.T) {
	mockStore := mocks.NewMockStore(t)

	repos := []*model.Repo{{ID: 1, FullName: "org/emailer"}}
	mockStore.On("RepoListAll", true, mock.Anything).Return(repos, nil)

	pipelines := []*model.Pipeline{
		{ID: 699, Number: 699, Status: model.StatusRunning, RepoID: 1},
	}
	mockStore.On("GetActivePipelineList", repos[0]).Return(pipelines, nil)
	mockStore.On("UpdatePipeline", mock.MatchedBy(func(p *model.Pipeline) bool {
		return p.ID == 699 && p.Status == model.StatusKilled
	})).Return(nil)
	mockStore.On("WorkflowGetTree", pipelines[0]).Return([]*model.Workflow{}, nil)

	r := NewOrphanReconciler()
	first := r.Tick(mockStore)
	second := r.Tick(mockStore)

	assert.Equal(t, 0, first)
	assert.Equal(t, 1, second, "second observation must trigger kill")
	assert.Equal(t, 0, r.PendingCount(), "killed pipeline must be removed from firstSeen")
}

// TestOrphanReconciler_RecoveredPipelinePruned: if a pipeline that was
// observed orphaned on tick N transitions to a terminal state by tick N+1
// (e.g. agent reconnected and finished it), the reconciler must drop it
// from firstSeen rather than carrying it forward indefinitely.
func TestOrphanReconciler_RecoveredPipelinePruned(t *testing.T) {
	mockStore := mocks.NewMockStore(t)

	repos := []*model.Repo{{ID: 1, FullName: "org/emailer"}}
	mockStore.On("RepoListAll", true, mock.Anything).Return(repos, nil)

	tick1Pipelines := []*model.Pipeline{
		{ID: 699, Number: 699, Status: model.StatusRunning, RepoID: 1},
	}
	tick2Pipelines := []*model.Pipeline{}

	mockStore.On("GetActivePipelineList", repos[0]).Return(tick1Pipelines, nil).Once()
	mockStore.On("GetActivePipelineList", repos[0]).Return(tick2Pipelines, nil).Once()

	r := NewOrphanReconciler()
	first := r.Tick(mockStore)
	second := r.Tick(mockStore)

	assert.Equal(t, 0, first)
	assert.Equal(t, 0, second)
	assert.Equal(t, 0, r.PendingCount(), "recovered pipeline must be pruned")
	mockStore.AssertNotCalled(t, "UpdatePipeline", mock.Anything)
}

// TestOrphanReconciler_NewOrphanGetsItsOwnGrace: a pipeline that first
// becomes orphaned on tick 2 must still get one grace tick — it should be
// killed on tick 3, not tick 2 just because some other pipeline already
// had an entry in firstSeen.
func TestOrphanReconciler_NewOrphanGetsItsOwnGrace(t *testing.T) {
	mockStore := mocks.NewMockStore(t)

	repos := []*model.Repo{{ID: 1, FullName: "org/emailer"}}
	mockStore.On("RepoListAll", true, mock.Anything).Return(repos, nil)

	tick1 := []*model.Pipeline{
		{ID: 699, Number: 699, Status: model.StatusRunning, RepoID: 1},
	}
	tick2 := []*model.Pipeline{
		{ID: 699, Number: 699, Status: model.StatusRunning, RepoID: 1},
		{ID: 700, Number: 700, Status: model.StatusRunning, RepoID: 1},
	}

	mockStore.On("GetActivePipelineList", repos[0]).Return(tick1, nil).Once()
	mockStore.On("GetActivePipelineList", repos[0]).Return(tick2, nil).Once()

	mockStore.On("UpdatePipeline", mock.MatchedBy(func(p *model.Pipeline) bool {
		return p.ID == 699 && p.Status == model.StatusKilled
	})).Return(nil)
	mockStore.On("WorkflowGetTree", mock.MatchedBy(func(p *model.Pipeline) bool {
		return p.ID == 699
	})).Return([]*model.Workflow{}, nil)

	r := NewOrphanReconciler()
	first := r.Tick(mockStore)
	second := r.Tick(mockStore)

	assert.Equal(t, 0, first)
	assert.Equal(t, 1, second, "only the older orphan should be killed on tick 2")
	assert.Equal(t, 1, r.PendingCount(), "the new orphan (700) must remain in grace")
}

// TestOrphanReconciler_SkipsNonRunning ensures we never kill pipelines that
// the queue is going to dispatch (StatusPending) or that are already terminal.
func TestOrphanReconciler_SkipsNonRunning(t *testing.T) {
	mockStore := mocks.NewMockStore(t)

	repos := []*model.Repo{{ID: 1, FullName: "org/repo"}}
	mockStore.On("RepoListAll", true, mock.Anything).Return(repos, nil)
	mockStore.On("GetActivePipelineList", repos[0]).Return([]*model.Pipeline{
		{ID: 1, Status: model.StatusPending, RepoID: 1},
		{ID: 2, Status: model.StatusBlocked, RepoID: 1},
	}, nil)

	r := NewOrphanReconciler()
	r.Tick(mockStore)
	r.Tick(mockStore)

	mockStore.AssertNotCalled(t, "UpdatePipeline", mock.Anything)
	assert.Equal(t, 0, r.PendingCount())
}

// TestOrphanReconciler_InjectedClock verifies the now() injection works for
// future test scenarios that need to exercise observed_for log fields.
func TestOrphanReconciler_InjectedClock(t *testing.T) {
	r := NewOrphanReconciler()
	fixed := time.Date(2026, 5, 9, 2, 12, 0, 0, time.UTC)
	r.now = func() time.Time { return fixed }
	assert.Equal(t, fixed, r.now())
}

// TestReconcileOrphanedAtStartup_RepoListErrorReturnsZero covers the
// defensive RepoListAll error path: log + early return 0.
func TestReconcileOrphanedAtStartup_RepoListErrorReturnsZero(t *testing.T) {
	mockStore := mocks.NewMockStore(t)
	mockStore.On("RepoListAll", true, mock.Anything).Return(nil, assert.AnError)

	count := ReconcileOrphanedAtStartup(mockStore)
	assert.Equal(t, 0, count)
	mockStore.AssertNotCalled(t, "GetActivePipelineList")
}

// TestReconcileOrphanedAtStartup_GetActiveErrorContinues covers the per-repo
// continue branch: GetActivePipelineList errors for one repo; the loop
// continues to the next repo without aborting the whole pass.
func TestReconcileOrphanedAtStartup_GetActiveErrorContinues(t *testing.T) {
	mockStore := mocks.NewMockStore(t)
	repos := []*model.Repo{
		{ID: 1, FullName: "org/broken"},
		{ID: 2, FullName: "org/works"},
	}
	mockStore.On("RepoListAll", true, mock.Anything).Return(repos, nil)
	mockStore.On("GetActivePipelineList", repos[0]).Return(nil, assert.AnError)
	mockStore.On("GetActivePipelineList", repos[1]).Return([]*model.Pipeline{}, nil)

	count := ReconcileOrphanedAtStartup(mockStore)
	assert.Equal(t, 0, count, "broken repo errors don't kill pipelines on the working repo either")
}

// TestReconcileOrphaned_BackCompatAlias verifies the alias still works.
func TestReconcileOrphaned_BackCompatAlias(t *testing.T) {
	mockStore := mocks.NewMockStore(t)
	mockStore.On("RepoListAll", true, mock.Anything).Return([]*model.Repo{}, nil)
	count := ReconcileOrphaned(mockStore)
	assert.Equal(t, 0, count)
}

// TestOrphanReconciler_TickRepoListError covers the timer-loop equivalent
// of the startup defensive path.
func TestOrphanReconciler_TickRepoListError(t *testing.T) {
	mockStore := mocks.NewMockStore(t)
	mockStore.On("RepoListAll", true, mock.Anything).Return(nil, assert.AnError)

	r := NewOrphanReconciler()
	count := r.Tick(mockStore)
	assert.Equal(t, 0, count)
}

// TestOrphanReconciler_TickGetActiveErrorContinues — same per-repo continue
// semantics as the startup version.
func TestOrphanReconciler_TickGetActiveErrorContinues(t *testing.T) {
	mockStore := mocks.NewMockStore(t)
	repos := []*model.Repo{{ID: 1, FullName: "org/broken"}}
	mockStore.On("RepoListAll", true, mock.Anything).Return(repos, nil)
	mockStore.On("GetActivePipelineList", repos[0]).Return(nil, assert.AnError)

	r := NewOrphanReconciler()
	count := r.Tick(mockStore)
	assert.Equal(t, 0, count)
}

// TestKillOrphan_UpdatePipelineFailureReturnsFalse covers the early-return
// path: UpdatePipeline errors → killOrphan returns false → reconcile count
// not incremented; workflow updates are NOT attempted.
func TestKillOrphan_UpdatePipelineFailureReturnsFalse(t *testing.T) {
	mockStore := mocks.NewMockStore(t)
	repos := []*model.Repo{{ID: 1, FullName: "org/r"}}
	mockStore.On("RepoListAll", true, mock.Anything).Return(repos, nil)
	mockStore.On("GetActivePipelineList", repos[0]).Return([]*model.Pipeline{
		{ID: 50, Number: 1, Status: model.StatusRunning, RepoID: 1},
	}, nil)
	mockStore.On("UpdatePipeline", mock.Anything).Return(assert.AnError)

	count := ReconcileOrphanedAtStartup(mockStore)
	assert.Equal(t, 0, count, "UpdatePipeline failure should not count as reconciled")
	mockStore.AssertNotCalled(t, "WorkflowGetTree")
}

// TestKillOrphan_WorkflowGetTreeFailureCountsAsKilled — UpdatePipeline
// succeeded so the kill counts; WorkflowGetTree errors short-circuit
// the workflow walk but the pipeline-kill itself is durable.
func TestKillOrphan_WorkflowGetTreeFailureCountsAsKilled(t *testing.T) {
	mockStore := mocks.NewMockStore(t)
	repos := []*model.Repo{{ID: 1, FullName: "org/r"}}
	mockStore.On("RepoListAll", true, mock.Anything).Return(repos, nil)
	mockStore.On("GetActivePipelineList", repos[0]).Return([]*model.Pipeline{
		{ID: 51, Number: 1, Status: model.StatusRunning, RepoID: 1},
	}, nil)
	mockStore.On("UpdatePipeline", mock.Anything).Return(nil)
	mockStore.On("WorkflowGetTree", mock.Anything).Return(nil, assert.AnError)

	count := ReconcileOrphanedAtStartup(mockStore)
	assert.Equal(t, 1, count, "pipeline kill counts even if workflow walk fails")
}

// TestKillOrphan_WorkflowUpdateFailureLoggedButContinues covers the
// WorkflowUpdate error path: failure is logged but doesn't block other
// workflow updates or the pipeline kill itself.
func TestKillOrphan_WorkflowUpdateFailureLoggedButContinues(t *testing.T) {
	mockStore := mocks.NewMockStore(t)
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
	// First WorkflowUpdate fails, second succeeds.
	mockStore.On("WorkflowUpdate", mock.MatchedBy(func(w *model.Workflow) bool {
		return w.ID == 1
	})).Return(assert.AnError)
	mockStore.On("WorkflowUpdate", mock.MatchedBy(func(w *model.Workflow) bool {
		return w.ID == 2
	})).Return(nil)

	count := ReconcileOrphanedAtStartup(mockStore)
	assert.Equal(t, 1, count, "pipeline still counts as reconciled despite per-workflow update failure")
}

// TestKillOrphan_PostsForgeStatusWhenManagerWired locks in the #181 fix:
// when a reconciler-driven kill happens, the forge gets a final status post
// so the upstream PR check transitions out of `pending`. fork#44's "skip
// on agent-disconnect" doesn't apply because reconcile kills don't set
// workflow.Error to a disconnect signature.
func TestKillOrphan_PostsForgeStatusWhenManagerWired(t *testing.T) {
	// Snapshot + restore the global to avoid polluting other tests.
	prevManager := server.Config.Services.Manager
	defer func() { server.Config.Services.Manager = prevManager }()

	mockStore := mocks.NewMockStore(t)
	mockForge := forge_mocks.NewMockForge(t)
	mockManager := services_mocks.NewMockManager(t)
	server.Config.Services.Manager = mockManager

	repos := []*model.Repo{{ID: 1, FullName: "org/r", UserID: 42}}
	mockStore.On("RepoListAll", true, mock.Anything).Return(repos, nil)
	mockStore.On("GetActivePipelineList", repos[0]).Return([]*model.Pipeline{
		{ID: 100, Number: 1, Status: model.StatusRunning, RepoID: 1},
	}, nil)
	mockStore.On("UpdatePipeline", mock.Anything).Return(nil)
	workflows := []*model.Workflow{
		{ID: 50, State: model.StatusRunning, Name: "ci"},
	}
	mockStore.On("WorkflowGetTree", mock.Anything).Return(workflows, nil)
	mockStore.On("WorkflowUpdate", mock.Anything).Return(nil)

	// User + forge lookup
	mockStore.On("GetUser", int64(42)).Return(&model.User{ID: 42, Login: "u"}, nil)
	mockManager.On("ForgeFromRepo", repos[0]).Return(mockForge, nil)
	mockForge.On("Refresh", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	mockForge.On("Status", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	count := ReconcileOrphanedAtStartup(mockStore)
	assert.Equal(t, 1, count)
	mockForge.AssertCalled(t, "Status", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// TestKillOrphan_NoForgeStatusWhenManagerMissing covers the test-context
// path: when server.Config.Services.Manager is nil (typical in unit tests),
// the kill still completes but no forge call is attempted. Fail-open
// behavior — kill is durable regardless of forge wiring.
func TestKillOrphan_NoForgeStatusWhenManagerMissing(t *testing.T) {
	prevManager := server.Config.Services.Manager
	defer func() { server.Config.Services.Manager = prevManager }()
	server.Config.Services.Manager = nil

	mockStore := mocks.NewMockStore(t)
	repos := []*model.Repo{{ID: 1, FullName: "org/r", UserID: 42}}
	mockStore.On("RepoListAll", true, mock.Anything).Return(repos, nil)
	mockStore.On("GetActivePipelineList", repos[0]).Return([]*model.Pipeline{
		{ID: 100, Number: 1, Status: model.StatusRunning, RepoID: 1},
	}, nil)
	mockStore.On("UpdatePipeline", mock.Anything).Return(nil)
	mockStore.On("WorkflowGetTree", mock.Anything).Return([]*model.Workflow{}, nil)

	count := ReconcileOrphanedAtStartup(mockStore)
	assert.Equal(t, 1, count, "kill completes even without a forge manager")
	// GetUser must NOT be called since we early-return before lookup.
	mockStore.AssertNotCalled(t, "GetUser")
}

// TestKillOrphan_ForgeLookupFailureLogged covers the path where the forge
// service can't be resolved — kill still completes; failure is logged.
func TestKillOrphan_ForgeLookupFailureLogged(t *testing.T) {
	prevManager := server.Config.Services.Manager
	defer func() { server.Config.Services.Manager = prevManager }()

	mockStore := mocks.NewMockStore(t)
	mockManager := services_mocks.NewMockManager(t)
	server.Config.Services.Manager = mockManager

	repos := []*model.Repo{{ID: 1, FullName: "org/r", UserID: 42}}
	mockStore.On("RepoListAll", true, mock.Anything).Return(repos, nil)
	mockStore.On("GetActivePipelineList", repos[0]).Return([]*model.Pipeline{
		{ID: 100, Number: 1, Status: model.StatusRunning, RepoID: 1},
	}, nil)
	mockStore.On("UpdatePipeline", mock.Anything).Return(nil)
	mockStore.On("WorkflowGetTree", mock.Anything).Return([]*model.Workflow{}, nil)
	mockStore.On("GetUser", int64(42)).Return(&model.User{ID: 42}, nil)
	mockManager.On("ForgeFromRepo", repos[0]).Return(nil, assert.AnError)

	count := ReconcileOrphanedAtStartup(mockStore)
	assert.Equal(t, 1, count, "kill completes even when forge lookup fails")
}

// TestKillOrphan_GetUserFailureLogged covers the parallel path where the
// repo's user can't be loaded — kill still completes; failure is logged.
func TestKillOrphan_GetUserFailureLogged(t *testing.T) {
	prevManager := server.Config.Services.Manager
	defer func() { server.Config.Services.Manager = prevManager }()

	mockStore := mocks.NewMockStore(t)
	mockManager := services_mocks.NewMockManager(t)
	server.Config.Services.Manager = mockManager

	repos := []*model.Repo{{ID: 1, FullName: "org/r", UserID: 42}}
	mockStore.On("RepoListAll", true, mock.Anything).Return(repos, nil)
	mockStore.On("GetActivePipelineList", repos[0]).Return([]*model.Pipeline{
		{ID: 100, Number: 1, Status: model.StatusRunning, RepoID: 1},
	}, nil)
	mockStore.On("UpdatePipeline", mock.Anything).Return(nil)
	mockStore.On("WorkflowGetTree", mock.Anything).Return([]*model.Workflow{}, nil)
	mockStore.On("GetUser", int64(42)).Return(nil, assert.AnError)

	count := ReconcileOrphanedAtStartup(mockStore)
	assert.Equal(t, 1, count)
}
