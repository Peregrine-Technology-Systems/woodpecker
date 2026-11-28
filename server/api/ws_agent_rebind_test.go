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

package api

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"go.woodpecker-ci.org/woodpecker/v3/rpc"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/pipeline"
	store_mocks "go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
	"go.woodpecker-ci.org/woodpecker/v3/server/store/types"
)

// #394: a reconnecting client names its prior agent id. The server re-binds
// that row only when it still exists and belongs to the same host; anything
// else falls through to a normal register.
func TestReboundAgent(t *testing.T) {
	const host = "ci-spot-us-eas-sfxz"

	t.Run("no prior id registers fresh", func(t *testing.T) {
		s := &wsAgentState{store: store_mocks.NewMockStore(t)}
		got, err := s.reboundAgent(RegisterPayload{}, host)
		assert.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("deleted row registers fresh", func(t *testing.T) {
		st := store_mocks.NewMockStore(t)
		st.On("AgentFind", int64(61493)).Return((*model.Agent)(nil), types.ErrRecordNotExist)
		s := &wsAgentState{store: st}
		got, err := s.reboundAgent(RegisterPayload{AgentID: 61493}, host)
		assert.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("another host's row is not taken", func(t *testing.T) {
		st := store_mocks.NewMockStore(t)
		st.On("AgentFind", int64(61493)).Return(&model.Agent{ID: 61493, Name: "other-host"}, nil)
		s := &wsAgentState{store: st}
		got, err := s.reboundAgent(RegisterPayload{AgentID: 61493}, host)
		assert.NoError(t, err)
		assert.Nil(t, got)
		st.AssertNotCalled(t, "AgentUpdate", mock.Anything)
	})

	t.Run("own row is re-bound with fresh details", func(t *testing.T) {
		st := store_mocks.NewMockStore(t)
		st.On("AgentFind", int64(61493)).Return(&model.Agent{ID: 61493, Name: host}, nil)
		st.On("AgentUpdate", mock.MatchedBy(func(a *model.Agent) bool {
			return a.ID == 61493 && a.Version == "pts.540" && a.Capacity == 2 && a.LastContact > 0
		})).Return(nil)
		s := &wsAgentState{store: st}
		got, err := s.reboundAgent(RegisterPayload{AgentID: 61493, Version: "pts.540", Capacity: 2}, host)
		assert.NoError(t, err)
		assert.Equal(t, int64(61493), got.ID)
	})

	t.Run("update failure keeps the row", func(t *testing.T) {
		st := store_mocks.NewMockStore(t)
		st.On("AgentFind", int64(61493)).Return(&model.Agent{ID: 61493, Name: host}, nil)
		st.On("AgentUpdate", mock.Anything).Return(errors.New("db busy"))
		s := &wsAgentState{store: st}
		got, err := s.reboundAgent(RegisterPayload{AgentID: 61493}, host)
		assert.NoError(t, err)
		assert.Equal(t, int64(61493), got.ID)
	})
}

// The ack code is decided by the error's type, never its text.
func TestAckCodeFor(t *testing.T) {
	terminal := fmt.Errorf("%w (state=killed)", pipeline.ErrStepUpdateRejectedTerminal)
	assert.Equal(t, rpc.AckCodeStepTerminal, ackCodeFor(terminal))
	assert.Empty(t, ackCodeFor(errors.New(pipeline.ErrStepUpdateRejectedTerminal.Error())),
		"same text, different type: untyped")
	assert.Empty(t, ackCodeFor(types.ErrRecordNotExist))
}

// The ack's typed code must survive the wire, and an unmarshalable payload is
// an error rather than an empty frame.
func TestNewEnvelope_CodeRoundTripAndMarshalError(t *testing.T) {
	data, err := newEnvelope(MsgAck, "r1", AckPayload{Error: "x", Code: rpc.AckCodeStepTerminal})
	assert.NoError(t, err)
	assert.Contains(t, string(data), `"code":"step_terminal"`)

	_, err = newEnvelope(MsgAck, "r2", make(chan int))
	assert.Error(t, err)
}
