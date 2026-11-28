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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/pipeline"
	"go.woodpecker-ci.org/woodpecker/v3/server/queue"
	store_mocks "go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
	"go.woodpecker-ci.org/woodpecker/v3/server/store/types"
)

// #418: an agent may only act on a workflow it holds. The queue says who holds
// a running workflow; once the workflow has left the queue (a late log batch,
// a re-sent Done), the agent recorded on the workflow at Init decides.

func ownerRPC(running ...*model.Task) *RPC {
	return &RPC{queue: &stubQueue{info: queue.InfoT{Running: running}}}
}

func TestCheckWorkflowOwner(t *testing.T) {
	caller := &model.Agent{ID: 5}
	wf := func(agentID int64) *model.Workflow { return &model.Workflow{ID: 42, AgentID: agentID} }
	heldBy := func(agentID int64) *model.Task { return &model.Task{ID: "42", AgentID: agentID} }

	for name, tc := range map[string]struct {
		running  []*model.Task
		workflow *model.Workflow
		mustHold bool
		allowed  bool
	}{
		"held by the caller":                           {running: []*model.Task{heldBy(5)}, workflow: wf(0), allowed: true},
		"held by the caller, must hold":                {running: []*model.Task{heldBy(5)}, workflow: wf(0), mustHold: true, allowed: true},
		"held by another agent":                        {running: []*model.Task{heldBy(6)}, workflow: wf(5)},
		"held by another agent, must hold":             {running: []*model.Task{heldBy(6)}, workflow: wf(5), mustHold: true},
		"held by no agent (dispatched elsewhere)":      {running: []*model.Task{heldBy(0)}, workflow: wf(5)},
		"left the queue, recorded on the caller":       {workflow: wf(5), allowed: true},
		"left the queue, recorded on another":          {workflow: wf(6)},
		"left the queue, never initialized":            {workflow: wf(0)},
		"not running, must hold (Init of an unpolled)": {workflow: wf(5), mustHold: true},
	} {
		t.Run(name, func(t *testing.T) {
			err := ownerRPC(tc.running...).checkWorkflowOwner(caller, tc.workflow, tc.mustHold)
			if tc.allowed {
				assert.NoError(t, err)
				return
			}
			assert.ErrorIs(t, err, ErrAgentNotWorkflowOwner)
			assert.ErrorContains(t, err, "workflow 42")
		})
	}
}

type brokenHolderQueue struct{ stubQueue }

func (brokenHolderQueue) Holder(string) (int64, error) { return 0, errors.New("queue unavailable") }

func TestCheckWorkflowOwnerSurfacesQueueErrors(t *testing.T) {
	s := &RPC{queue: &brokenHolderQueue{}}

	err := s.checkWorkflowOwner(&model.Agent{ID: 5}, &model.Workflow{ID: 42, AgentID: 5}, false)

	assert.ErrorContains(t, err, "queue unavailable")
	assert.NotErrorIs(t, err, ErrAgentNotWorkflowOwner, "an unknown answer is not a refusal")
}

// A refused step update tells the agent to abandon the workflow, in the form
// agents from pts.634 on (WS) and pts.642 on (gRPC) already obey.
func TestNotOwnerUpdateRejectionReadsAsAbandoned(t *testing.T) {
	err := ownerRPC(&model.Task{ID: "42", AgentID: 6}).checkWorkflowOwner(&model.Agent{ID: 5}, &model.Workflow{ID: 42}, false)
	require.Error(t, err)

	rejection := abandonedRejection(err)

	assert.ErrorIs(t, rejection, ErrAgentNotWorkflowOwner)
	assert.ErrorIs(t, rejection, pipeline.ErrStepUpdateRejectedTerminal)
	var terr *pipeline.StepTerminalError
	require.ErrorAs(t, rejection, &terr)
	assert.Equal(t, model.StatusKilled, terr.State)
}

func TestCheckStepInWorkflow(t *testing.T) {
	workflow := &model.Workflow{PipelineID: 10, PID: 2}

	assert.NoError(t, checkStepInWorkflow(&model.Step{UUID: "s", PipelineID: 10, PPID: 2}, workflow))
	assert.ErrorContains(t, checkStepInWorkflow(&model.Step{UUID: "s", PipelineID: 10, PPID: 3}, workflow),
		"does not belong to workflow", "a sibling workflow's step in the same pipeline")
	assert.ErrorContains(t, checkStepInWorkflow(&model.Step{UUID: "s", PipelineID: 11, PPID: 2}, workflow),
		"does not belong to workflow", "another pipeline's step")
}

func TestCheckWorkflowOwnerByIDRejectsBadInput(t *testing.T) {
	s := store_mocks.NewMockStore(t)
	s.On("WorkflowLoad", int64(43)).Return(nil, types.ErrRecordNotExist)
	r := &RPC{queue: &stubQueue{}, store: s}

	assert.Error(t, r.checkWorkflowOwnerByID(&model.Agent{ID: 5}, "not-a-number"))
	assert.ErrorContains(t, r.checkWorkflowOwnerByID(&model.Agent{ID: 5}, "43"), "cannot find workflow 43")
}
