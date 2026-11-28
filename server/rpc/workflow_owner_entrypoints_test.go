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
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	grpcMetadata "google.golang.org/grpc/metadata"

	"go.woodpecker-ci.org/woodpecker/v3/rpc"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/pipeline"
	"go.woodpecker-ci.org/woodpecker/v3/server/queue"
	store_mocks "go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
)

// #418 through the real RPC entry points: agent 5 acts on workflow 42, which
// the queue says agent 6 holds. Every call is refused, and refused before it
// changes anything: the store mock has no write expectation, so a write fails
// the test.

const (
	ownerTestStep = "step-of-wf-42"
	ownerTestWF   = "42"
)

func ownerEntrypointRPC(t *testing.T, holder int64) *RPC {
	t.Helper()
	s := store_mocks.NewMockStore(t)
	for _, id := range []int64{5, 6} {
		s.On("AgentFind", id).Return(&model.Agent{ID: id, OwnerID: model.IDNotSet, OrgID: model.IDNotSet}, nil).Maybe()
	}
	wf := &model.Workflow{ID: 42, PipelineID: 100, PID: 1, AgentID: holder}
	s.On("WorkflowLoad", int64(42)).Return(wf, nil).Maybe()
	s.On("WorkflowByStep", mock.Anything).Return(wf, nil).Maybe()
	s.On("StepListFromWorkflowFind", mock.Anything).Return([]*model.Step{}, nil).Maybe()
	s.On("GetPipeline", int64(100)).Return(&model.Pipeline{ID: 100, RepoID: 9, Status: model.StatusRunning}, nil).Maybe()
	s.On("GetRepo", int64(9)).Return(&model.Repo{ID: 9, FullName: "org/repo"}, nil).Maybe()
	s.On("StepByUUID", ownerTestStep).Return(&model.Step{ID: 1, UUID: ownerTestStep, PipelineID: 100, PPID: 1, State: model.StatusRunning}, nil).Maybe()
	s.On("AgentUpdate", mock.Anything).Return(nil).Maybe() // last-work stamp, not workflow state
	return &RPC{queue: &stubQueue{info: queue.InfoT{Running: []*model.Task{{ID: ownerTestWF, AgentID: holder}}}}, store: s}
}

func asAgent(id string) context.Context {
	return grpcMetadata.NewIncomingContext(context.Background(), grpcMetadata.Pairs("agent_id", id))
}

func TestAnAgentCannotActOnAWorkflowAnotherAgentHolds(t *testing.T) {
	calls := map[string]func(*RPC) error{
		"Init": func(s *RPC) error { return s.Init(asAgent("5"), ownerTestWF, rpc.WorkflowState{Started: 1}) },
		"Update": func(s *RPC) error {
			return s.Update(asAgent("5"), ownerTestWF, rpc.StepState{StepUUID: ownerTestStep, Exited: true})
		},
		"Done":   func(s *RPC) error { return s.Done(asAgent("5"), ownerTestWF, rpc.WorkflowState{Finished: 2}) },
		"Extend": func(s *RPC) error { return s.Extend(asAgent("5"), ownerTestWF) },
		"Wait":   func(s *RPC) error { _, err := s.Wait(asAgent("5"), ownerTestWF); return err },
		"Log": func(s *RPC) error {
			return s.Log(asAgent("5"), ownerTestStep, []*rpc.LogEntry{{StepUUID: ownerTestStep, Data: []byte("x")}})
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			err := call(ownerEntrypointRPC(t, 6))
			assert.ErrorIs(t, err, ErrAgentNotWorkflowOwner)
		})
	}
}

// The refused step update reaches the agent as "the server killed this
// step", so the agent abandons the workflow instead of retrying.
func TestARefusedStepUpdateTellsTheAgentToAbandon(t *testing.T) {
	err := ownerEntrypointRPC(t, 6).Update(asAgent("5"), ownerTestWF, rpc.StepState{StepUUID: ownerTestStep})

	var terr *pipeline.StepTerminalError
	require.ErrorAs(t, err, &terr)
	assert.Equal(t, model.StatusKilled, terr.State)
}

func TestTheHolderMayExtendAndWait(t *testing.T) {
	s := ownerEntrypointRPC(t, 5)

	assert.NoError(t, s.Extend(asAgent("5"), ownerTestWF))
	_, err := s.Wait(asAgent("5"), ownerTestWF)
	assert.NoError(t, err)
}

// A step of a sibling workflow in the same pipeline is refused even for the
// holder of this workflow (upstream #7547).
func TestAStepOfAnotherWorkflowIsRefused(t *testing.T) {
	s := ownerEntrypointRPC(t, 5)
	ms := s.store.(*store_mocks.MockStore)
	ms.On("StepByUUID", "sibling-step").Return(&model.Step{ID: 2, UUID: "sibling-step", PipelineID: 100, PPID: 2}, nil)

	err := s.Update(asAgent("5"), ownerTestWF, rpc.StepState{StepUUID: "sibling-step"})

	assert.ErrorContains(t, err, "does not belong to workflow 42")
}
