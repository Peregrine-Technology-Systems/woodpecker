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
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"go.woodpecker-ci.org/woodpecker/v3/server"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/pubsub"
	queue_mocks "go.woodpecker-ci.org/woodpecker/v3/server/queue/mocks"
	"go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
)

// =============================================================================
// Unit tests: findIndependentWorkflows
// =============================================================================

func TestFindIndependentWorkflows(t *testing.T) {
	t.Parallel()

	t.Run("identifies workflows with no dependencies", func(t *testing.T) {
		t.Parallel()
		workflows := []*model.Workflow{
			{ID: 100, Name: "ci", DependsOn: nil},
			{ID: 101, Name: "deploy", DependsOn: []string{}},
			{ID: 102, Name: "promote", DependsOn: []string{"ci"}},
		}

		result := findIndependentWorkflows(workflows)
		assert.True(t, result[100], "workflow with nil DependsOn should be independent")
		assert.True(t, result[101], "workflow with empty DependsOn should be independent")
		assert.False(t, result[102], "workflow with deps should not be independent")
	})

	t.Run("returns empty map for no workflows", func(t *testing.T) {
		t.Parallel()
		result := findIndependentWorkflows(nil)
		assert.Empty(t, result)
	})

	t.Run("all workflows independent when none have dependencies", func(t *testing.T) {
		t.Parallel()
		workflows := []*model.Workflow{
			{ID: 1, Name: "ci", DependsOn: nil},
			{ID: 2, Name: "deploy", DependsOn: []string{}},
		}

		result := findIndependentWorkflows(workflows)
		assert.True(t, result[1])
		assert.True(t, result[2])
	})

	// Regression test: the old implementation used TaskList() which only returns
	// tasks still in the queue. A running workflow's task has been dequeued by
	// the agent (Poll deletes from task store), so it was invisible and got killed.
	// Backend pipeline #3336 was killed this way.
	t.Run("running workflow preserved even after task dequeued", func(t *testing.T) {
		t.Parallel()
		workflows := []*model.Workflow{
			{ID: 100, Name: "ci", State: model.StatusSuccess, DependsOn: nil},
			{ID: 101, Name: "deploy", State: model.StatusRunning, DependsOn: []string{}},
			{ID: 102, Name: "promote", State: model.StatusPending, DependsOn: []string{"ci"}},
		}

		result := findIndependentWorkflows(workflows)
		assert.True(t, result[100], "completed CI should be independent")
		assert.True(t, result[101], "running deploy must be independent — this was the bug")
		assert.False(t, result[102], "promote depends on CI")
	})
}

func TestFindIndependentWorkflows_BackwardsCompat(t *testing.T) {
	t.Parallel()

	// Existing pipelines created before the DependsOn migration won't have
	// the field set. All workflows will have nil DependsOn, making them all
	// appear independent. This is the safe default — better to preserve a
	// workflow that should have been killed than to kill one that should be
	// preserved.
	t.Run("pre-migration workflows all appear independent", func(t *testing.T) {
		t.Parallel()
		workflows := []*model.Workflow{
			{ID: 1, Name: "ci", DependsOn: nil},
			{ID: 2, Name: "deploy", DependsOn: nil},
			{ID: 3, Name: "promote", DependsOn: nil},
		}

		result := findIndependentWorkflows(workflows)
		assert.True(t, result[1])
		assert.True(t, result[2])
		assert.True(t, result[3])
	})
}

// =============================================================================
// Integration tests: Cancel with preserveIndependent
// =============================================================================

// setupCancelTest wires up mock store + queue + pubsub for Cancel tests.
func setupCancelTest(t *testing.T) (*mocks.MockStore, *queue_mocks.MockQueue) {
	t.Helper()

	mockStore := mocks.NewMockStore(t)
	mockQueue := queue_mocks.NewMockQueue(t)

	server.Config.Services.Queue = mockQueue
	server.Config.Services.Pubsub = pubsub.New()

	return mockStore, mockQueue
}

// TestCancel_PreserveIndependent_RunningDeploy is the integration test that
// reproduces the exact production bug from backend pipeline #3336.
//
// Scenario: Pipeline has 3 workflows:
//   - ci (depends_on: nil) — already finished
//   - deploy (depends_on: []) — RUNNING on an agent (task dequeued from queue)
//   - promote (depends_on: ["ci"]) — still pending
//
// A new push triggers cancelPreviousPipelines -> Cancel(preserveIndependent=true).
// Expected: deploy stays running, promote gets canceled, pipeline stays running.
func TestCancel_PreserveIndependent_RunningDeploy(t *testing.T) {
	mockStore, mockQueue := setupCancelTest(t)

	pipeline := &model.Pipeline{
		ID:     1,
		Number: 100,
		Status: model.StatusRunning,
	}

	repo := &model.Repo{ID: 1, FullName: "org/backend"}
	user := &model.User{ID: 1}

	// Workflows as they exist in the DB (persisted DependsOn)
	workflows := []*model.Workflow{
		{
			ID:         10,
			PipelineID: 1,
			Name:       "ci",
			State:      model.StatusSuccess,
			DependsOn:  nil,
		},
		{
			ID:         11,
			PipelineID: 1,
			Name:       "deploy",
			State:      model.StatusRunning,
			DependsOn:  []string{},
			Children:   []*model.Step{},
		},
		{
			ID:         12,
			PipelineID: 1,
			Name:       "promote",
			State:      model.StatusPending,
			DependsOn:  []string{"ci"},
			Children: []*model.Step{
				{ID: 120, State: model.StatusPending},
			},
		},
	}

	mockStore.On("WorkflowGetTree", pipeline).Return(workflows, nil)

	// Only promote (ID=12) should be evicted from queue — not deploy (ID=11)
	mockQueue.On("ErrorAtOnce", mock.Anything, []string{"12"}, mock.Anything).Return(nil)

	// promote workflow -> skipped
	mockStore.On("WorkflowUpdate", mock.MatchedBy(func(w *model.Workflow) bool {
		return w.ID == 12 && w.State == model.StatusSkipped
	})).Return(nil)

	// promote's pending step -> canceled
	mockStore.On("StepUpdate", mock.MatchedBy(func(s *model.Step) bool {
		return s.ID == 120 && s.State == model.StatusCanceled
	})).Return(nil)

	// Pipeline should NOT be killed — deploy is still running
	// (no call to UpdatePipeline expected)

	cancelInfo := &model.CancelInfo{SupersededBy: 101}
	err := Cancel(context.Background(), nil, mockStore, repo, user, pipeline, cancelInfo, true)

	assert.NoError(t, err)
	// Pipeline status should NOT have been updated to killed
	mockStore.AssertNotCalled(t, "UpdatePipeline", mock.Anything)
}

// TestCancel_PreserveIndependent_NoRunningIndependent kills pipeline when
// all independent workflows have finished and only dependent ones remain pending.
func TestCancel_PreserveIndependent_NoRunningIndependent(t *testing.T) {
	mockStore, mockQueue := setupCancelTest(t)

	pipeline := &model.Pipeline{
		ID:     2,
		Number: 200,
		Status: model.StatusRunning,
	}
	repo := &model.Repo{ID: 1, FullName: "org/backend"}
	user := &model.User{ID: 1}

	workflows := []*model.Workflow{
		{
			ID:        20,
			Name:      "ci",
			State:     model.StatusSuccess,
			DependsOn: nil,
		},
		{
			ID:        21,
			Name:      "promote",
			State:     model.StatusPending,
			DependsOn: []string{"ci"},
			Children: []*model.Step{
				{ID: 210, State: model.StatusPending},
			},
		},
	}

	mockStore.On("WorkflowGetTree", pipeline).Return(workflows, nil)

	// promote evicted
	mockQueue.On("ErrorAtOnce", mock.Anything, []string{"21"}, mock.Anything).Return(nil)

	// promote -> skipped
	mockStore.On("WorkflowUpdate", mock.MatchedBy(func(w *model.Workflow) bool {
		return w.ID == 21 && w.State == model.StatusSkipped
	})).Return(nil)

	// promote step -> canceled
	mockStore.On("StepUpdate", mock.MatchedBy(func(s *model.Step) bool {
		return s.ID == 210 && s.State == model.StatusCanceled
	})).Return(nil)

	// No independent workflows still running -> pipeline superseded (woodpecker-server#7).
	// hasPendingOnly=true (promote is only pending), so status=canceled not superseded.
	mockStore.On("UpdatePipeline", mock.MatchedBy(func(p *model.Pipeline) bool {
		return p.ID == 2 && p.Status == model.StatusCanceled
	})).Return(nil)

	// Second WorkflowGetTree call after kill (for publishToTopic)
	mockStore.On("WorkflowGetTree", mock.AnythingOfType("*model.Pipeline")).Return(workflows, nil)

	cancelInfo := &model.CancelInfo{SupersededBy: 201}
	err := Cancel(context.Background(), nil, mockStore, repo, user, pipeline, cancelInfo, true)

	assert.NoError(t, err)
	mockStore.AssertCalled(t, "UpdatePipeline", mock.Anything)
}

// TestCancel_NoPreserve kills all workflows regardless of dependencies.
func TestCancel_NoPreserve(t *testing.T) {
	mockStore, mockQueue := setupCancelTest(t)

	pipeline := &model.Pipeline{
		ID:     3,
		Number: 300,
		Status: model.StatusRunning,
	}
	repo := &model.Repo{ID: 1, FullName: "org/backend"}
	user := &model.User{ID: 1}

	workflows := []*model.Workflow{
		{
			ID:        30,
			Name:      "ci",
			State:     model.StatusRunning,
			DependsOn: nil,
			Children:  []*model.Step{},
		},
		{
			ID:        31,
			Name:      "deploy",
			State:     model.StatusRunning,
			DependsOn: []string{},
			Children:  []*model.Step{},
		},
	}

	mockStore.On("WorkflowGetTree", pipeline).Return(workflows, nil)

	// Both running workflows evicted — preserveIndependent=false
	mockQueue.On("ErrorAtOnce", mock.Anything, mock.MatchedBy(func(ids []string) bool {
		return len(ids) == 2
	}), mock.Anything).Return(nil)

	// #891: Done() called for both running workflows to clean up queue
	mockQueue.On("Done", mock.Anything, "30", model.StatusKilled).Return(nil)
	mockQueue.On("Done", mock.Anything, "31", model.StatusKilled).Return(nil)

	// Pipeline superseded (not pending-only since both are running, SupersededBy > 0)
	mockStore.On("UpdatePipeline", mock.MatchedBy(func(p *model.Pipeline) bool {
		return p.ID == 3 && p.Status == model.StatusSuperseded
	})).Return(nil)

	// Second WorkflowGetTree after kill
	mockStore.On("WorkflowGetTree", mock.AnythingOfType("*model.Pipeline")).Return(workflows, nil)

	cancelInfo := &model.CancelInfo{SupersededBy: 301}
	err := Cancel(context.Background(), nil, mockStore, repo, user, pipeline, cancelInfo, false)

	assert.NoError(t, err)
	mockStore.AssertCalled(t, "UpdatePipeline", mock.Anything)
}

// TestCancel_RunningWorkflow_QueueDone ensures that canceling a pipeline with
// running workflows calls queue.Done() in addition to ErrorAtOnce().
// Without this, ghost tasks stay in the queue's running map when the agent is
// dead — ErrorAtOnce only removes pending/waiting tasks, not running ones that
// the agent should have cleaned up via rpc.Done() (#891).
func TestCancel_RunningWorkflow_QueueDone(t *testing.T) {
	mockStore, mockQueue := setupCancelTest(t)

	pipeline := &model.Pipeline{
		ID:     5,
		Number: 500,
		Status: model.StatusRunning,
	}
	repo := &model.Repo{ID: 1, FullName: "org/scanner"}
	user := &model.User{ID: 1}

	workflows := []*model.Workflow{
		{
			ID:        50,
			Name:      "ci",
			State:     model.StatusRunning,
			DependsOn: []string{},
			Children:  []*model.Step{},
		},
	}

	mockStore.On("WorkflowGetTree", pipeline).Return(workflows, nil)

	// ErrorAtOnce called for the running workflow
	mockQueue.On("ErrorAtOnce", mock.Anything, []string{"50"}, mock.Anything).Return(nil)

	// #891: queue.Done() must ALSO be called for running workflows to ensure
	// the task is removed from the queue's running map even if the agent is dead
	mockQueue.On("Done", mock.Anything, "50", model.StatusKilled).Return(nil)

	// Pipeline killed
	mockStore.On("UpdatePipeline", mock.MatchedBy(func(p *model.Pipeline) bool {
		return p.ID == 5 && p.Status == model.StatusKilled
	})).Return(nil)

	mockStore.On("WorkflowGetTree", mock.AnythingOfType("*model.Pipeline")).Return(workflows, nil)

	err := Cancel(context.Background(), nil, mockStore, repo, user, pipeline, &model.CancelInfo{
		CanceledByUser: "system",
	}, false)

	assert.NoError(t, err)
	// Verify Done was called — this is the critical assertion
	mockQueue.AssertCalled(t, "Done", mock.Anything, "50", model.StatusKilled)
}

// TestCancel_RejectsNonRunningPipeline verifies guard clause.
func TestCancel_RejectsNonRunningPipeline(t *testing.T) {
	pipeline := &model.Pipeline{
		ID:     4,
		Status: model.StatusSuccess,
	}
	repo := &model.Repo{ID: 1}
	user := &model.User{ID: 1}

	err := Cancel(context.Background(), nil, nil, repo, user, pipeline, nil, false)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "Cannot cancel")
}

// =============================================================================
// #193 — kill-path audit logging
// =============================================================================

// captureLogs swaps log.Logger to a buffer-backed JSON logger for the duration
// of one test. It returns the buffer and a restore func.
func captureLogs(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := log.Logger
	log.Logger = zerolog.New(buf).With().Logger()
	return buf, func() { log.Logger = prev }
}

// findCancelLog scans captured log output for the audit line emitted by
// Cancel/killOrphan and returns its parsed JSON.
func findCancelLog(t *testing.T, buf *bytes.Buffer) map[string]interface{} {
	t.Helper()
	for _, line := range strings.Split(buf.String(), "\n") {
		if !strings.Contains(line, "pipeline cancel: transitioning") {
			continue
		}
		var rec map[string]interface{}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("could not parse log line %q: %v", line, err)
		}
		return rec
	}
	return nil
}

// TestCancel_AuditLog_PendingOnly captures the mode-B case from #190: pipeline
// canceled while still pending, never dispatched. The audit log must emit
// reason=pending_only_canceled and never_dispatched=true (#193).
func TestCancel_AuditLog_PendingOnly(t *testing.T) {
	buf, restore := captureLogs(t)
	defer restore()

	mockStore, mockQueue := setupCancelTest(t)
	pipeline := &model.Pipeline{ID: 9001, Number: 9, Status: model.StatusPending, Started: 0}
	repo := &model.Repo{ID: 1, FullName: "org/audit"}
	user := &model.User{ID: 1}
	workflows := []*model.Workflow{
		{ID: 90, Name: "ci", State: model.StatusPending, Children: []*model.Step{}},
	}

	mockStore.On("WorkflowGetTree", mock.Anything).Return(workflows, nil)
	mockQueue.On("ErrorAtOnce", mock.Anything, []string{"90"}, mock.Anything).Return(nil)
	mockStore.On("WorkflowUpdate", mock.Anything).Return(nil)
	mockStore.On("UpdatePipeline", mock.MatchedBy(func(p *model.Pipeline) bool {
		return p.Status == model.StatusCanceled
	})).Return(nil)

	err := Cancel(context.Background(), nil, mockStore, repo, user, pipeline, nil, false)
	assert.NoError(t, err)

	rec := findCancelLog(t, buf)
	if assert.NotNil(t, rec, "expected audit log line in output: %q", buf.String()) {
		assert.Equal(t, float64(9001), rec["pipeline_id"])
		assert.Equal(t, "org/audit", rec["repo"])
		assert.Equal(t, "pending", rec["prior_status"])
		assert.Equal(t, "canceled", rec["new_status"])
		assert.Equal(t, true, rec["never_dispatched"])
		assert.Equal(t, "pending_only_canceled", rec["reason"])
	}
}

// TestCancel_AuditLog_Superseded captures the cancelPreviousPipelines path:
// SupersededBy set on cancelInfo. Audit must emit reason=superseded_by_newer_push.
func TestCancel_AuditLog_Superseded(t *testing.T) {
	buf, restore := captureLogs(t)
	defer restore()

	mockStore, mockQueue := setupCancelTest(t)
	pipeline := &model.Pipeline{ID: 9002, Number: 10, Status: model.StatusRunning, Started: 12345}
	repo := &model.Repo{ID: 1, FullName: "org/audit"}
	user := &model.User{ID: 1}
	workflows := []*model.Workflow{
		{ID: 91, Name: "ci", State: model.StatusRunning, Children: []*model.Step{}},
	}

	mockStore.On("WorkflowGetTree", mock.Anything).Return(workflows, nil)
	mockQueue.On("ErrorAtOnce", mock.Anything, []string{"91"}, mock.Anything).Return(nil)
	mockQueue.On("Done", mock.Anything, "91", model.StatusKilled).Return(nil)
	mockStore.On("UpdatePipeline", mock.MatchedBy(func(p *model.Pipeline) bool {
		return p.Status == model.StatusSuperseded
	})).Return(nil)

	err := Cancel(context.Background(), nil, mockStore, repo, user, pipeline, &model.CancelInfo{SupersededBy: 11}, false)
	assert.NoError(t, err)

	rec := findCancelLog(t, buf)
	if assert.NotNil(t, rec, "expected audit log line in output: %q", buf.String()) {
		assert.Equal(t, "superseded", rec["new_status"])
		assert.Equal(t, false, rec["never_dispatched"])
		assert.Equal(t, "superseded_by_newer_push", rec["reason"])
	}
}

// =============================================================================
// cancelPreviousPipelines (was 0% pre-#193; touching cancel.go means owning it)
// =============================================================================

// TestCancelPreviousPipelines_EventNotIncluded — repo has cancel-previous
// configured but for different events; this push must be a no-op.
func TestCancelPreviousPipelines_EventNotIncluded(t *testing.T) {
	mockStore, _ := setupCancelTest(t)
	repo := &model.Repo{ID: 1, FullName: "org/repo", CancelPreviousPipelineEvents: []model.WebhookEvent{model.EventPull}}
	pl := &model.Pipeline{ID: 1, Event: model.EventPush, Branch: "main"}
	user := &model.User{ID: 1}

	err := cancelPreviousPipelines(context.Background(), nil, mockStore, pl, repo, user)
	assert.NoError(t, err)
	mockStore.AssertNotCalled(t, "GetActivePipelineList", mock.Anything)
}

// TestCancelPreviousPipelines_SkipsSelfAndDifferentEvent — walks the active
// list, skips the self-pipeline and any pipeline whose event differs.
func TestCancelPreviousPipelines_SkipsSelfAndDifferentEvent(t *testing.T) {
	mockStore, _ := setupCancelTest(t)
	repo := &model.Repo{ID: 1, FullName: "org/repo", CancelPreviousPipelineEvents: []model.WebhookEvent{model.EventPush}}
	newPL := &model.Pipeline{ID: 999, Number: 50, Event: model.EventPush, Branch: "main"}
	user := &model.User{ID: 1}

	mockStore.On("GetActivePipelineList", repo).Return([]*model.Pipeline{
		{ID: 999, Event: model.EventPush, Branch: "main"}, // same → skip self
		{ID: 998, Event: model.EventPull, Branch: "main"}, // different event → skip
	}, nil)

	err := cancelPreviousPipelines(context.Background(), nil, mockStore, newPL, repo, user)
	assert.NoError(t, err)
	// Cancel never invoked: no WorkflowGetTree call expected.
	mockStore.AssertNotCalled(t, "WorkflowGetTree", mock.Anything)
}

// TestCancelPreviousPipelines_BranchMismatchSkipped — same event, same repo,
// but different branch → not canceled.
func TestCancelPreviousPipelines_BranchMismatchSkipped(t *testing.T) {
	mockStore, _ := setupCancelTest(t)
	repo := &model.Repo{ID: 1, FullName: "org/repo", CancelPreviousPipelineEvents: []model.WebhookEvent{model.EventPush}}
	newPL := &model.Pipeline{ID: 1000, Number: 60, Event: model.EventPush, Branch: "main"}
	user := &model.User{ID: 1}

	mockStore.On("GetActivePipelineList", repo).Return([]*model.Pipeline{
		{ID: 1001, Event: model.EventPush, Branch: "feature/x"}, // different branch
	}, nil)

	err := cancelPreviousPipelines(context.Background(), nil, mockStore, newPL, repo, user)
	assert.NoError(t, err)
	mockStore.AssertNotCalled(t, "WorkflowGetTree", mock.Anything)
}

// TestCancelPreviousPipelines_BranchMatchCancels — the canonical case: a new
// push on `main` cancels the in-flight pipeline on `main`.
func TestCancelPreviousPipelines_BranchMatchCancels(t *testing.T) {
	mockStore, mockQueue := setupCancelTest(t)
	repo := &model.Repo{ID: 1, FullName: "org/repo", CancelPreviousPipelineEvents: []model.WebhookEvent{model.EventPush}}
	newPL := &model.Pipeline{ID: 2000, Number: 70, Event: model.EventPush, Branch: "main"}
	user := &model.User{ID: 1}

	priorPL := &model.Pipeline{ID: 1999, Number: 69, Event: model.EventPush, Branch: "main", Status: model.StatusRunning}
	mockStore.On("GetActivePipelineList", repo).Return([]*model.Pipeline{priorPL}, nil)

	// Cancel(prior) walks workflows; one running, one independent.
	workflows := []*model.Workflow{
		{ID: 700, Name: "ci", State: model.StatusRunning, DependsOn: []string{}, Children: []*model.Step{}},
	}
	mockStore.On("WorkflowGetTree", priorPL).Return(workflows, nil)
	// preserveIndependent=true → DependsOn:[] keeps the deploy alive.
	// hasPreservedRunning=true → no UpdatePipeline call expected.

	err := cancelPreviousPipelines(context.Background(), nil, mockStore, newPL, repo, user)
	assert.NoError(t, err)

	// Confirm we actually entered Cancel: WorkflowGetTree was called on prior.
	mockStore.AssertCalled(t, "WorkflowGetTree", priorPL)
	// preserveIndependent kept the deploy: no eviction, no kill.
	mockQueue.AssertNotCalled(t, "ErrorAtOnce", mock.Anything, mock.Anything, mock.Anything)
	mockStore.AssertNotCalled(t, "UpdatePipeline", mock.Anything)
}

// TestCancelPreviousPipelines_StoreError surfaces the store-error early-return.
func TestCancelPreviousPipelines_StoreError(t *testing.T) {
	mockStore, _ := setupCancelTest(t)
	repo := &model.Repo{ID: 1, FullName: "org/repo", CancelPreviousPipelineEvents: []model.WebhookEvent{model.EventPush}}
	newPL := &model.Pipeline{ID: 3000, Event: model.EventPush, Branch: "main"}
	user := &model.User{ID: 1}

	mockStore.On("GetActivePipelineList", repo).Return(nil, assert.AnError)
	err := cancelPreviousPipelines(context.Background(), nil, mockStore, newPL, repo, user)
	assert.Error(t, err)
}

// TestCancel_WorkflowGetTreeError covers the error path on line 42.
func TestCancel_WorkflowGetTreeError(t *testing.T) {
	mockStore, _ := setupCancelTest(t)
	pl := &model.Pipeline{ID: 4000, Status: model.StatusRunning}
	repo := &model.Repo{ID: 1, FullName: "org/r"}
	user := &model.User{ID: 1}

	mockStore.On("WorkflowGetTree", pl).Return(nil, assert.AnError)
	err := Cancel(context.Background(), nil, mockStore, repo, user, pl, nil, false)
	assert.Error(t, err)
	_, isNotFound := err.(*ErrNotFound)
	assert.True(t, isNotFound, "expected ErrNotFound, got %T", err)
}

// TestCancelPreviousPipelines_RefspecMatchForPullEvent covers the default
// (non-push) branch in pipelineNeedsCancel — a new PR push compares Refspec
// instead of Branch.
func TestCancelPreviousPipelines_RefspecMatchForPullEvent(t *testing.T) {
	mockStore, _ := setupCancelTest(t)
	repo := &model.Repo{ID: 1, FullName: "org/repo", CancelPreviousPipelineEvents: []model.WebhookEvent{model.EventPull}}
	newPL := &model.Pipeline{ID: 5000, Number: 80, Event: model.EventPull, Refspec: "refs/pull/9/head:refs/pull/9/head"}
	user := &model.User{ID: 1}
	prior := &model.Pipeline{ID: 4999, Event: model.EventPull, Refspec: "refs/pull/9/head:refs/pull/9/head", Status: model.StatusRunning}
	other := &model.Pipeline{ID: 4998, Event: model.EventPull, Refspec: "refs/pull/8/head:refs/pull/8/head"} // unrelated PR
	mockStore.On("GetActivePipelineList", repo).Return([]*model.Pipeline{prior, other}, nil)
	mockStore.On("WorkflowGetTree", prior).Return([]*model.Workflow{
		{ID: 800, Name: "ci", State: model.StatusSuccess}, // already-finished, doesn't trigger queue ops
	}, nil)
	// hasPendingOnly=true since no Pending/Running, plState becomes Canceled.
	mockStore.On("UpdatePipeline", mock.Anything).Return(nil)
	mockStore.On("WorkflowGetTree", mock.AnythingOfType("*model.Pipeline")).Return([]*model.Workflow{}, nil)

	err := cancelPreviousPipelines(context.Background(), nil, mockStore, newPL, repo, user)
	assert.NoError(t, err)
	mockStore.AssertCalled(t, "WorkflowGetTree", prior)
	// 'other' has different Refspec → no Cancel call (no extra WorkflowGetTree).
}

// TestCancel_AllErrorPaths exercises the inner error-log branches: queue ops
// failing, workflow update failing, step update failing, and final
// UpdateToStatusKilled failing. Each is a logged-and-continue (or returned)
// path that should not panic and should not corrupt state.
func TestCancel_AllErrorPaths(t *testing.T) {
	t.Run("ErrorAtOnce_failure_logged", func(t *testing.T) {
		mockStore, mockQueue := setupCancelTest(t)
		pl := &model.Pipeline{ID: 6001, Status: model.StatusRunning}
		repo := &model.Repo{ID: 1, FullName: "org/r"}
		user := &model.User{ID: 1}
		mockStore.On("WorkflowGetTree", pl).Return([]*model.Workflow{
			{ID: 600, State: model.StatusRunning, DependsOn: []string{"missing"}, Children: []*model.Step{}},
		}, nil)
		mockQueue.On("ErrorAtOnce", mock.Anything, mock.Anything, mock.Anything).Return(assert.AnError)
		mockQueue.On("Done", mock.Anything, mock.Anything, mock.Anything).Return(nil)
		mockStore.On("UpdatePipeline", mock.Anything).Return(nil)
		mockStore.On("WorkflowGetTree", mock.AnythingOfType("*model.Pipeline")).Return([]*model.Workflow{}, nil)
		err := Cancel(context.Background(), nil, mockStore, repo, user, pl, nil, false)
		assert.NoError(t, err) // log.Error but don't bail
	})
	t.Run("Queue_Done_failure_logged", func(t *testing.T) {
		mockStore, mockQueue := setupCancelTest(t)
		pl := &model.Pipeline{ID: 6002, Status: model.StatusRunning}
		repo := &model.Repo{ID: 1, FullName: "org/r"}
		user := &model.User{ID: 1}
		mockStore.On("WorkflowGetTree", pl).Return([]*model.Workflow{
			{ID: 601, State: model.StatusRunning, DependsOn: []string{"missing"}, Children: []*model.Step{}},
		}, nil)
		mockQueue.On("ErrorAtOnce", mock.Anything, mock.Anything, mock.Anything).Return(nil)
		mockQueue.On("Done", mock.Anything, "601", model.StatusKilled).Return(assert.AnError)
		mockStore.On("UpdatePipeline", mock.Anything).Return(nil)
		mockStore.On("WorkflowGetTree", mock.AnythingOfType("*model.Pipeline")).Return([]*model.Workflow{}, nil)
		err := Cancel(context.Background(), nil, mockStore, repo, user, pl, nil, false)
		assert.NoError(t, err)
	})
	t.Run("WorkflowUpdate_failure_logged", func(t *testing.T) {
		mockStore, mockQueue := setupCancelTest(t)
		pl := &model.Pipeline{ID: 6003, Status: model.StatusPending}
		repo := &model.Repo{ID: 1, FullName: "org/r"}
		user := &model.User{ID: 1}
		mockStore.On("WorkflowGetTree", pl).Return([]*model.Workflow{
			{ID: 602, State: model.StatusPending, DependsOn: []string{"missing"}, Children: []*model.Step{
				{ID: 6020, State: model.StatusPending},
			}},
		}, nil)
		mockQueue.On("ErrorAtOnce", mock.Anything, mock.Anything, mock.Anything).Return(nil)
		// Both inner updates fail — Cancel logs and continues.
		mockStore.On("WorkflowUpdate", mock.Anything).Return(assert.AnError)
		mockStore.On("StepUpdate", mock.Anything).Return(assert.AnError)
		mockStore.On("UpdatePipeline", mock.Anything).Return(nil)
		mockStore.On("WorkflowGetTree", mock.AnythingOfType("*model.Pipeline")).Return([]*model.Workflow{}, nil)
		err := Cancel(context.Background(), nil, mockStore, repo, user, pl, nil, false)
		assert.NoError(t, err)
	})
	t.Run("UpdateToStatusKilled_returns_error", func(t *testing.T) {
		mockStore, mockQueue := setupCancelTest(t)
		pl := &model.Pipeline{ID: 6004, Status: model.StatusRunning}
		repo := &model.Repo{ID: 1, FullName: "org/r"}
		user := &model.User{ID: 1}
		mockStore.On("WorkflowGetTree", pl).Return([]*model.Workflow{
			{ID: 603, State: model.StatusRunning, DependsOn: []string{"missing"}, Children: []*model.Step{}},
		}, nil)
		mockQueue.On("ErrorAtOnce", mock.Anything, mock.Anything, mock.Anything).Return(nil)
		mockQueue.On("Done", mock.Anything, mock.Anything, mock.Anything).Return(nil)
		mockStore.On("UpdatePipeline", mock.Anything).Return(assert.AnError)
		err := Cancel(context.Background(), nil, mockStore, repo, user, pl, nil, false)
		assert.Error(t, err) // bubble up
	})
	t.Run("PostKill_WorkflowGetTree_returns_error", func(t *testing.T) {
		mockStore, mockQueue := setupCancelTest(t)
		pl := &model.Pipeline{ID: 6005, Status: model.StatusRunning}
		repo := &model.Repo{ID: 1, FullName: "org/r"}
		user := &model.User{ID: 1}
		mockStore.On("WorkflowGetTree", pl).Return([]*model.Workflow{
			{ID: 604, State: model.StatusRunning, DependsOn: []string{"missing"}, Children: []*model.Step{}},
		}, nil).Once()
		mockQueue.On("ErrorAtOnce", mock.Anything, mock.Anything, mock.Anything).Return(nil)
		mockQueue.On("Done", mock.Anything, mock.Anything, mock.Anything).Return(nil)
		mockStore.On("UpdatePipeline", mock.Anything).Return(nil)
		// Second WorkflowGetTree (for publishToTopic) errors — Cancel returns it.
		mockStore.On("WorkflowGetTree", mock.AnythingOfType("*model.Pipeline")).Return(nil, assert.AnError).Once()
		err := Cancel(context.Background(), nil, mockStore, repo, user, pl, nil, false)
		assert.Error(t, err)
	})
}

// TestCancelPreviousPipelines_CancelError covers the inner Cancel-error log
// branch in cancelPreviousPipelines (lines 228-234).
func TestCancelPreviousPipelines_CancelError(t *testing.T) {
	mockStore, _ := setupCancelTest(t)
	repo := &model.Repo{ID: 1, FullName: "org/r", CancelPreviousPipelineEvents: []model.WebhookEvent{model.EventPush}}
	newPL := &model.Pipeline{ID: 7001, Event: model.EventPush, Branch: "main"}
	user := &model.User{ID: 1}
	prior := &model.Pipeline{ID: 7000, Status: model.StatusRunning, Event: model.EventPush, Branch: "main"}
	mockStore.On("GetActivePipelineList", repo).Return([]*model.Pipeline{prior}, nil)
	// Cancel(prior) fails because WorkflowGetTree errors → log.Error but
	// cancelPreviousPipelines does NOT bubble the failure (just logs it).
	mockStore.On("WorkflowGetTree", prior).Return(nil, assert.AnError)
	err := cancelPreviousPipelines(context.Background(), nil, mockStore, newPL, repo, user)
	assert.NoError(t, err)
}

// TestCancel_AuditLog_UserInitiated covers the default branch (running pipeline,
// no SupersededBy, has running workflows). Audit must emit reason=user_initiated.
func TestCancel_AuditLog_UserInitiated(t *testing.T) {
	buf, restore := captureLogs(t)
	defer restore()

	mockStore, mockQueue := setupCancelTest(t)
	pipeline := &model.Pipeline{ID: 9003, Number: 11, Status: model.StatusRunning, Started: 67890}
	repo := &model.Repo{ID: 1, FullName: "org/audit"}
	user := &model.User{ID: 1}
	workflows := []*model.Workflow{
		{ID: 92, Name: "ci", State: model.StatusRunning, Children: []*model.Step{}},
	}

	mockStore.On("WorkflowGetTree", mock.Anything).Return(workflows, nil)
	mockQueue.On("ErrorAtOnce", mock.Anything, []string{"92"}, mock.Anything).Return(nil)
	mockQueue.On("Done", mock.Anything, "92", model.StatusKilled).Return(nil)
	mockStore.On("UpdatePipeline", mock.MatchedBy(func(p *model.Pipeline) bool {
		return p.Status == model.StatusKilled
	})).Return(nil)

	err := Cancel(context.Background(), nil, mockStore, repo, user, pipeline, &model.CancelInfo{CanceledByUser: "alice"}, false)
	assert.NoError(t, err)

	rec := findCancelLog(t, buf)
	if assert.NotNil(t, rec, "expected audit log line in output: %q", buf.String()) {
		assert.Equal(t, "killed", rec["new_status"])
		assert.Equal(t, "user_initiated", rec["reason"])
	}
}
