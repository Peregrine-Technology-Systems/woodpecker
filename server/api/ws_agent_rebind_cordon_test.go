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
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"go.woodpecker-ci.org/woodpecker/v3/server"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	queue_mocks "go.woodpecker-ci.org/woodpecker/v3/server/queue/mocks"
	store_mocks "go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
	"go.woodpecker-ci.org/woodpecker/v3/server/store/types"
)

// #440: the scaler cordons an agent with PATCH {"no_schedule": true}. PatchAgent
// copied the missing name as "", so the next reconnect after a server restart
// failed reboundAgent's hostname check and minted a fresh id, orphaning the
// agent's running task.

func patchAgent(t *testing.T, stored *model.Agent, body string) *model.Agent {
	t.Helper()
	gin.SetMode(gin.TestMode)
	st := store_mocks.NewMockStore(t)
	st.On("AgentFind", stored.ID).Return(stored, nil)
	var saved *model.Agent
	st.On("AgentUpdate", mock.Anything).Return(nil).Run(func(a mock.Arguments) { saved = a.Get(0).(*model.Agent) })
	q := queue_mocks.NewMockQueue(t)
	q.On("KickAgentWorkers", mock.Anything).Maybe()
	prev := server.Config.Services.Queue
	server.Config.Services.Queue = q
	t.Cleanup(func() { server.Config.Services.Queue = prev })

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("store", st)
	c.Params = gin.Params{{Key: "agent_id", Value: "64080"}}
	c.Request, _ = http.NewRequest(http.MethodPatch, "/", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	PatchAgent(c)
	require.Equal(t, http.StatusOK, w.Code)
	require.NotNil(t, saved)
	return saved
}

func TestPatchAgent_CordonKeepsTheName(t *testing.T) {
	got := patchAgent(t, &model.Agent{ID: 64080, Name: "ci-spot-host"}, `{"no_schedule":true}`)

	assert.True(t, got.NoSchedule)
	assert.Equal(t, "ci-spot-host", got.Name, "a cordon must not blank the agent's name")
}

func TestPatchAgent_RenameKeepsTheCordon(t *testing.T) {
	got := patchAgent(t, &model.Agent{ID: 64080, Name: "old", NoSchedule: true}, `{"name":"new"}`)

	assert.Equal(t, "new", got.Name)
	assert.True(t, got.NoSchedule, "a rename must not uncordon the agent")
}

func TestPatchAgent_UncordonClearsIt(t *testing.T) {
	got := patchAgent(t, &model.Agent{ID: 64080, Name: "h", NoSchedule: true}, `{"no_schedule":false}`)

	assert.False(t, got.NoSchedule)
	assert.Equal(t, "h", got.Name)
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Logger
	log.Logger = zerolog.New(&buf)
	t.Cleanup(func() { log.Logger = prev })
	return &buf
}

// #440 ask 1: every refusal says why.
func TestReboundAgent_LogsWhyItDidNotRebind(t *testing.T) {
	const host = "ci-spot-host"
	for name, tc := range map[string]struct {
		agent  *model.Agent
		err    error
		reason string
	}{
		"no row":        {nil, types.ErrRecordNotExist, "no row"},
		"name mismatch": {&model.Agent{ID: 64080, Name: "other-host"}, nil, "name mismatch"},
	} {
		t.Run(name, func(t *testing.T) {
			buf := captureLog(t)
			st := store_mocks.NewMockStore(t)
			st.On("AgentFind", int64(64080)).Return(tc.agent, tc.err)

			got, err := (&wsAgentState{store: st}).reboundAgent(RegisterPayload{AgentID: 64080}, host)

			require.NoError(t, err)
			assert.Nil(t, got)
			assert.Contains(t, buf.String(), tc.reason)
			assert.Contains(t, buf.String(), `"prior_agent_id":64080`)
		})
	}
}

// #440 ask 2: a find error is not "no such agent". Fail the registration so the
// agent retries, rather than mint a fresh id that orphans its running task.
func TestReboundAgent_AFindErrorFailsTheRegistration(t *testing.T) {
	buf := captureLog(t)
	st := store_mocks.NewMockStore(t)
	st.On("AgentFind", int64(64080)).Return((*model.Agent)(nil), errors.New("database is locked"))

	got, err := (&wsAgentState{store: st}).reboundAgent(RegisterPayload{AgentID: 64080}, "ci-spot-host")

	assert.Nil(t, got)
	require.ErrorContains(t, err, "database is locked")
	assert.Contains(t, buf.String(), "find error")
}

// A row the old PatchAgent already blanked cannot prove whose it is: system
// agents share one token and have no org. So no hostname may take it, the
// agent's own included; it registers fresh, as before #440 (ENG's condition).
func TestReboundAgent_ABlankedRowIsNotTakenByAnyHost(t *testing.T) {
	for _, host := range []string{"ci-spot-host", "another-host"} {
		t.Run(host, func(t *testing.T) {
			buf := captureLog(t)
			st := store_mocks.NewMockStore(t)
			st.On("AgentFind", int64(64080)).Return(&model.Agent{ID: 64080, Name: ""}, nil)

			got, err := (&wsAgentState{store: st}).reboundAgent(RegisterPayload{AgentID: 64080}, host)

			require.NoError(t, err)
			assert.Nil(t, got)
			assert.Contains(t, buf.String(), "blank name")
			st.AssertNotCalled(t, "AgentUpdate", mock.Anything)
		})
	}
}
