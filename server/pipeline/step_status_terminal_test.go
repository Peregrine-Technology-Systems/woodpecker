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
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.woodpecker-ci.org/woodpecker/v3/rpc"
	"go.woodpecker-ci.org/woodpecker/v3/server"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	services_mocks "go.woodpecker-ci.org/woodpecker/v3/server/services/mocks"
	"go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
)

// #394: the terminal rejection carries the step's server-side state as a typed
// field, so the agent can tell "the server abandoned this work" (killed,
// canceled) from "the server already has this result" (a re-sent final update
// whose first ack was lost) without parsing the message.
func TestUpdateStepStatus_TerminalRejectionCarriesState(t *testing.T) {
	for _, st := range []model.StatusValue{model.StatusKilled, model.StatusSuccess} {
		step := &model.Step{State: st}
		err := UpdateStepStatus(context.Background(), nil, step, rpc.StepState{Exited: true})

		var terr *StepTerminalError
		require.True(t, errors.As(err, &terr), "state %s", st)
		assert.Equal(t, st, terr.State)
		assert.ErrorIs(t, err, ErrStepUpdateRejectedTerminal)
		assert.Equal(t, "step is in terminal state and does not expect rpc state updates (state="+string(st)+")", err.Error())
	}
}

// The fail-cancel path (a failing step with `failure: cancel` cancels its
// pipeline) surfaces every lookup failure instead of swallowing it. Covered
// here because #394 took ownership of step_status.go.
func TestUpdateStepStatus_FailCancelSurfacesLookupErrors(t *testing.T) {
	prev := server.Config.Services.Manager
	t.Cleanup(func() { server.Config.Services.Manager = prev })

	boom := errors.New("boom")
	cases := map[string]func(*mocks.MockStore, *services_mocks.MockManager){
		"pipeline": func(s *mocks.MockStore, _ *services_mocks.MockManager) {
			s.On("GetPipeline", int64(100)).Return(nil, boom)
		},
		"repo": func(s *mocks.MockStore, _ *services_mocks.MockManager) {
			s.On("GetPipeline", int64(100)).Return(&model.Pipeline{ID: 100, RepoID: 9}, nil)
			s.On("GetRepo", int64(9)).Return(nil, boom)
		},
		"user": func(s *mocks.MockStore, _ *services_mocks.MockManager) {
			s.On("GetPipeline", int64(100)).Return(&model.Pipeline{ID: 100, RepoID: 9}, nil)
			s.On("GetRepo", int64(9)).Return(&model.Repo{ID: 9, UserID: 4}, nil)
			s.On("GetUser", int64(4)).Return(nil, boom)
		},
		"forge": func(s *mocks.MockStore, m *services_mocks.MockManager) {
			repo := &model.Repo{ID: 9, UserID: 4}
			s.On("GetPipeline", int64(100)).Return(&model.Pipeline{ID: 100, RepoID: 9}, nil)
			s.On("GetRepo", int64(9)).Return(repo, nil)
			s.On("GetUser", int64(4)).Return(&model.User{ID: 4}, nil)
			m.On("ForgeFromRepo", repo).Return(nil, boom)
		},
	}
	for name, seed := range cases {
		for _, from := range []model.StatusValue{model.StatusPending, model.StatusRunning} {
			t.Run(name+"/"+string(from), func(t *testing.T) {
				s := mocks.NewMockStore(t)
				m := services_mocks.NewMockManager(t)
				server.Config.Services.Manager = m
				seed(s, m)
				step := &model.Step{PipelineID: 100, State: from, Failure: model.FailureCancel}
				err := UpdateStepStatus(context.Background(), s, step, rpc.StepState{Exited: true, ExitCode: 1})
				assert.ErrorIs(t, err, boom)
			})
		}
	}
}
