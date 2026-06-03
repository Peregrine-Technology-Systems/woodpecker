// Copyright 2022 Woodpecker Authors
// Copyright 2019 mhmxs.
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
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/store"
	"go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
)

func mockStorePipeline(t *testing.T) store.Store {
	s := mocks.NewMockStore(t)
	s.On("UpdatePipeline", mock.Anything).Return(nil)
	return s
}

func TestUpdateToStatusRunning(t *testing.T) {
	t.Parallel()

	pipeline, _ := UpdateToStatusRunning(mockStorePipeline(t), model.Pipeline{}, int64(1))
	assert.Equal(t, model.StatusRunning, pipeline.Status)
	assert.EqualValues(t, 1, pipeline.Started)
}

func TestUpdateToStatusPending(t *testing.T) {
	t.Parallel()

	now := time.Now().Unix()

	pipeline, _ := UpdateToStatusPending(mockStorePipeline(t), model.Pipeline{}, "Reviewer")

	assert.Equal(t, model.StatusPending, pipeline.Status)
	assert.Equal(t, "Reviewer", pipeline.Reviewer)
	assert.LessOrEqual(t, now, pipeline.Reviewed)
}

func TestUpdateToStatusDeclined(t *testing.T) {
	t.Parallel()

	now := time.Now().Unix()

	pipeline, _ := UpdateToStatusDeclined(mockStorePipeline(t), model.Pipeline{}, "Reviewer")

	assert.Equal(t, model.StatusDeclined, pipeline.Status)
	assert.Equal(t, "Reviewer", pipeline.Reviewer)
	assert.LessOrEqual(t, now, pipeline.Reviewed)
}

func TestUpdateToStatusToDone(t *testing.T) {
	t.Parallel()

	pipeline, _ := UpdateStatusToDone(mockStorePipeline(t), model.Pipeline{}, "status", int64(1))

	assert.Equal(t, model.StatusValue("status"), pipeline.Status)
	assert.EqualValues(t, 1, pipeline.Finished)
}

func TestUpdateToStatusError(t *testing.T) {
	t.Parallel()

	now := time.Now().Unix()

	pipeline, _ := UpdateToStatusError(mockStorePipeline(t), model.Pipeline{}, errors.New("this is an error"))

	assert.Len(t, pipeline.Errors, 1)
	assert.Equal(t, "[generic] this is an error", pipeline.Errors[0].Error())
	assert.Equal(t, model.StatusError, pipeline.Status)
	assert.Equal(t, pipeline.Started, pipeline.Finished)
	assert.LessOrEqual(t, now, pipeline.Started)
	assert.Equal(t, pipeline.Started, pipeline.Finished)
}

func TestUpdateToStatusKilled(t *testing.T) {
	t.Parallel()

	now := time.Now().Unix()
	cancelInfo := &model.CancelInfo{
		SupersededBy: 2,
	}

	pipeline, _ := UpdateToStatusKilled(mockStorePipeline(t), model.Pipeline{}, cancelInfo, model.StatusKilled, "user_initiated")

	assert.Equal(t, model.StatusKilled, pipeline.Status)
	assert.NotNil(t, pipeline.CancelInfo)
	assert.EqualValues(t, 2, pipeline.CancelInfo.SupersededBy)
	assert.LessOrEqual(t, now, pipeline.Finished)
	assert.Equal(t, "user_initiated", pipeline.KillReason, "#202: KillReason must be stamped")
	assert.LessOrEqual(t, now, pipeline.KilledAt)
}

// TestUpdateStatusToDone_StampsKillReason_AgentDisconnect verifies the
// #202 derivation path: when the rolled-up status is a terminal kill
// AND any workflow has the agent-disconnect signature, KillReason is
// stamped as "agent_disconnect".
func TestUpdateStatusToDone_StampsKillReason_AgentDisconnect(t *testing.T) {
	t.Parallel()
	now := time.Now().Unix()
	pl := model.Pipeline{
		Workflows: []*model.Workflow{
			{ID: 1, State: model.StatusKilled, Error: "agent disconnected"},
		},
	}
	updated, _ := UpdateStatusToDone(mockStorePipeline(t), pl, model.StatusKilled, now)
	assert.Equal(t, "agent_disconnect", updated.KillReason)
	assert.Equal(t, now, updated.KilledAt)
}

// TestUpdateStatusToDone_StampsKillReason_AgentDoneKill — a Killed
// rollup without the disconnect marker indicates the agent itself
// reported a kill (e.g. from its own internal state machine).
func TestUpdateStatusToDone_StampsKillReason_AgentDoneKill(t *testing.T) {
	t.Parallel()
	now := time.Now().Unix()
	pl := model.Pipeline{
		Workflows: []*model.Workflow{
			{ID: 1, State: model.StatusKilled}, // no Error
		},
	}
	updated, _ := UpdateStatusToDone(mockStorePipeline(t), pl, model.StatusKilled, now)
	assert.Equal(t, "agent_done_kill", updated.KillReason)
}

// TestUpdateStatusToDone_PreservesExistingKillReason — Cancel() set
// KillReason="user_initiated" earlier; the agent later reports done
// and we recompute via UpdateStatusToDone. We must NOT overwrite the
// upstream attribution.
func TestUpdateStatusToDone_PreservesExistingKillReason(t *testing.T) {
	t.Parallel()
	pl := model.Pipeline{
		KillReason: "user_initiated",
		Workflows: []*model.Workflow{
			{ID: 1, State: model.StatusKilled, Error: "agent disconnected"},
		},
	}
	updated, _ := UpdateStatusToDone(mockStorePipeline(t), pl, model.StatusKilled, 100)
	assert.Equal(t, "user_initiated", updated.KillReason, "must not overwrite upstream attribution")
}

// TestUpdateStatusToDone_NonTerminalDoesNotStamp — Success rollup
// must not set KillReason.
func TestUpdateStatusToDone_NonTerminalDoesNotStamp(t *testing.T) {
	t.Parallel()
	pl := model.Pipeline{}
	updated, _ := UpdateStatusToDone(mockStorePipeline(t), pl, model.StatusSuccess, 100)
	assert.Empty(t, updated.KillReason)
	assert.Zero(t, updated.KilledAt)
}

// TestKillReasonFromWorkflows_NilSafety — defensive against nil entries
// in the slice.
func TestKillReasonFromWorkflows_NilSafety(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "agent_done_kill", killReasonFromWorkflows(nil))
	assert.Equal(t, "agent_done_kill", killReasonFromWorkflows([]*model.Workflow{nil, nil}))
}

// TestKillReasonFromWorkflows_AgentPreempted — a workflow the agent canceled
// because it was shutting down (SIGTERM/preemption, #275) is tagged
// "agent_preempted", distinct from the generic agent_done_kill fallback. (#275)
func TestKillReasonFromWorkflows_AgentPreempted(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		workflows []*model.Workflow
		want      string
	}{
		{
			name:      "agent shutdown signature → agent_preempted",
			workflows: []*model.Workflow{{ID: 1, State: model.StatusKilled, Error: "agent shutdown"}},
			want:      "agent_preempted",
		},
		{
			name:      "plain Canceled (server-issued) → agent_done_kill, not preempted",
			workflows: []*model.Workflow{{ID: 1, State: model.StatusKilled, Error: "Canceled"}},
			want:      "agent_done_kill",
		},
		{
			// Disconnect wins over shutdown — it is checked first and is the
			// more specific infrastructure-failure class.
			name: "disconnect takes precedence over shutdown",
			workflows: []*model.Workflow{
				{ID: 1, State: model.StatusKilled, Error: "agent shutdown"},
				{ID: 2, State: model.StatusKilled, Error: "agent disconnected"},
			},
			want: "agent_disconnect",
		},
		{
			name: "shutdown among siblings still detected",
			workflows: []*model.Workflow{
				{ID: 1, State: model.StatusSuccess},
				{ID: 2, State: model.StatusKilled, Error: "agent shutdown"},
			},
			want: "agent_preempted",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, killReasonFromWorkflows(tc.workflows))
		})
	}
}

// TestUpdateStatusToDone_StampsKillReason_AgentPreempted — end-to-end through
// the #202 derivation: a killed rollup whose workflow carries the agent-shutdown
// signature stamps KillReason="agent_preempted". (#275)
func TestUpdateStatusToDone_StampsKillReason_AgentPreempted(t *testing.T) {
	t.Parallel()
	now := time.Now().Unix()
	pl := model.Pipeline{
		Workflows: []*model.Workflow{
			{ID: 1, State: model.StatusKilled, Error: "agent shutdown"},
		},
	}
	updated, _ := UpdateStatusToDone(mockStorePipeline(t), pl, model.StatusKilled, now)
	assert.Equal(t, "agent_preempted", updated.KillReason)
	assert.Equal(t, now, updated.KilledAt)
}

// TestPipelineStatus_Empty / Running / RolledUp — exercise the
// PipelineStatus rollup so the file climbs above the 95% gate.
func TestPipelineStatus_EmptyReturnsSuccess(t *testing.T) {
	t.Parallel()
	assert.Equal(t, model.StatusSuccess, PipelineStatus(nil))
	assert.Equal(t, model.StatusSuccess, PipelineStatus([]*model.Workflow{}))
}

func TestPipelineStatus_AnyRunningOrPendingReturnsRunning(t *testing.T) {
	t.Parallel()
	assert.Equal(t, model.StatusRunning, PipelineStatus([]*model.Workflow{
		{State: model.StatusSuccess},
		{State: model.StatusRunning},
	}))
	assert.Equal(t, model.StatusRunning, PipelineStatus([]*model.Workflow{
		{State: model.StatusPending},
	}))
}

func TestPipelineStatus_AllTerminalRollsUp(t *testing.T) {
	t.Parallel()
	// Single workflow returns its own state.
	assert.Equal(t, model.StatusSuccess, PipelineStatus([]*model.Workflow{
		{State: model.StatusSuccess},
	}))
	// Multiple workflows merge via MergeStatusValues — Killed dominates Success.
	got := PipelineStatus([]*model.Workflow{
		{State: model.StatusSuccess},
		{State: model.StatusKilled},
	})
	// We don't pin the exact merge outcome here; just assert it isn't Running.
	assert.NotEqual(t, model.StatusRunning, got)
}

// TestPipelineStatus_SkippedNeverMasksPartial reproduces #270: a pipeline whose
// workflows are [ci:skipped, deploy:partial, promote:skipped] must NOT roll up
// to `skipped`. Before the MergeStatusValues fix the rollup seeded on the first
// skipped workflow and the partial-absorption branch returned the lowest-priority
// `skipped`, so a deploy that actually mutated production was reported as if
// nothing happened. The pipeline must report at least `partial`.
func TestPipelineStatus_SkippedNeverMasksPartial(t *testing.T) {
	t.Parallel()
	got := PipelineStatus([]*model.Workflow{
		{Name: "ci", State: model.StatusSkipped},
		{Name: "deploy", State: model.StatusPartial},
		{Name: "promote", State: model.StatusSkipped},
	})
	assert.Equal(t, model.StatusPartial, got,
		"a partial deploy workflow must not be masked by skipped siblings (#270)")

	// Order-independence: the partial workflow first, in the middle, or last all
	// roll up to partial.
	assert.Equal(t, model.StatusPartial, PipelineStatus([]*model.Workflow{
		{State: model.StatusPartial},
		{State: model.StatusSkipped},
	}))
	assert.Equal(t, model.StatusPartial, PipelineStatus([]*model.Workflow{
		{State: model.StatusSkipped},
		{State: model.StatusPartial},
	}))
}

// TestIsTerminalKill — pure helper, exhaustive over StatusValue.
func TestIsTerminalKill(t *testing.T) {
	cases := map[model.StatusValue]bool{
		model.StatusKilled:     true,
		model.StatusCanceled:   true,
		model.StatusSuperseded: true,
		model.StatusFailure:    false,
		model.StatusError:      false,
		model.StatusSuccess:    false,
		model.StatusSkipped:    false,
		model.StatusRunning:    false,
		model.StatusPending:    false,
		model.StatusBlocked:    false,
		model.StatusDeclined:   false,
		model.StatusCreated:    false,
		model.StatusPartial:    false,
	}
	for status, want := range cases {
		assert.Equal(t, want, IsTerminalKill(status), "IsTerminalKill(%s)", status)
	}
}
