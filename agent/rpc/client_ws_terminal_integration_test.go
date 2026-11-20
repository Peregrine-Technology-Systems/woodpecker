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

package rpc

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"go.woodpecker-ci.org/woodpecker/v3/rpc"
	"go.woodpecker-ci.org/woodpecker/v3/server"
	"go.woodpecker-ci.org/woodpecker/v3/server/api"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/queue"
	queue_mocks "go.woodpecker-ci.org/woodpecker/v3/server/queue/mocks"
	grpcserver "go.woodpecker-ci.org/woodpecker/v3/server/rpc"
	store_mocks "go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
	"go.woodpecker-ci.org/woodpecker/v3/server/store/types"
)

// #394 part 2, end to end: the REAL agent WSClient against the REAL server
// WSAgent handler and RPC. Only the store and queue are fakes. The store keeps
// an agents table, so a lookup by an id it never issued fails the way the real
// one does (sql: no rows), which is how the incident surfaced.

const killedStepUUID = "01M7WN8VWY93-killed"

type terminalFixture struct {
	url        string
	agentsMu   sync.Mutex
	agents     map[int64]*model.Agent
	nextID     int64
	creates    atomic.Int64
	stepReads  atomic.Int64
	polledWith chan int64
}

func newTerminalFixture(t *testing.T) *terminalFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	server.Config.Server.AgentToken = "test-secret"
	f := &terminalFixture{agents: map[int64]*model.Agent{}, nextID: 61493, polledWith: make(chan int64, 8)}

	s := store_mocks.NewMockStore(t)
	s.On("AgentCreate", mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		a := args.Get(0).(*model.Agent)
		f.agentsMu.Lock()
		a.ID = f.nextID
		f.nextID++
		f.agents[a.ID] = a
		f.agentsMu.Unlock()
		f.creates.Add(1)
	}).Maybe()
	s.On("AgentFind", mock.AnythingOfType("int64")).Return(func(id int64) (*model.Agent, error) {
		f.agentsMu.Lock()
		defer f.agentsMu.Unlock()
		if a, ok := f.agents[id]; ok {
			return a, nil
		}
		return nil, types.ErrRecordNotExist
	}).Maybe()
	s.On("AgentUpdate", mock.Anything).Return(nil).Maybe()
	s.On("AgentList", mock.Anything).Return(func(*model.ListOptions) ([]*model.Agent, error) {
		f.agentsMu.Lock()
		defer f.agentsMu.Unlock()
		out := make([]*model.Agent, 0, len(f.agents))
		for _, a := range f.agents {
			out = append(out, a)
		}
		return out, nil
	}).Maybe()
	s.On("WorkflowLoad", mock.Anything).Return(&model.Workflow{ID: 117374, PipelineID: 100}, nil).Maybe()
	s.On("GetPipeline", mock.Anything).Return(&model.Pipeline{ID: 100, RepoID: 9, Status: model.StatusKilled}, nil).Maybe()
	s.On("GetRepo", mock.Anything).Return(&model.Repo{ID: 9, FullName: "org/infra"}, nil).Maybe()
	s.On("StepByUUID", killedStepUUID).Return(&model.Step{
		ID: 1, UUID: killedStepUUID, PipelineID: 100, State: model.StatusKilled,
	}, nil).Run(func(mock.Arguments) { f.stepReads.Add(1) }).Maybe()

	q := queue_mocks.NewMockQueue(t)
	data, _ := json.Marshal(rpc.Workflow{ID: "117400"})
	q.On("Poll", mock.Anything, mock.AnythingOfType("int64"), mock.Anything).Return(
		func(_ context.Context, agentID int64, _ queue.FilterFn) (*model.Task, error) {
			f.polledWith <- agentID
			return &model.Task{ID: "117400", Data: data}, nil
		}).Maybe()
	q.On("Wait", mock.Anything, mock.Anything).Return(nil).Maybe()
	q.On("Info", mock.Anything).Return(queue.InfoT{}).Maybe()
	q.On("KickAgentWorkers", mock.Anything).Maybe()

	server.Config.Services.WSAgentRPC = grpcserver.NewRPCForTesting(q, s)
	t.Cleanup(func() { server.Config.Services.WSAgentRPC = nil })

	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("store", s); c.Next() })
	r.GET("/ws/agent", api.WSAgent)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

func registeredClient(t *testing.T, f *terminalFixture) (*WSClient, int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wsTestDeadline)
	defer cancel()
	c := NewWSClient(context.Background(), f.url[len("http://"):], "test-secret", "ci-spot-us-eas-sfxz", false).(*WSClient)
	id, err := c.RegisterAgent(ctx, rpc.AgentInfo{Capacity: 1, Backend: "docker", Platform: "linux/amd64"})
	require.NoError(t, err)
	return c, id
}

func updateKilledStep(c *WSClient) error {
	ctx, cancel := context.WithTimeout(context.Background(), wsTestDeadline)
	defer cancel()
	return c.Update(ctx, "117374", rpc.StepState{StepUUID: killedStepUUID, Exited: true, ExitCode: 0})
}

func assertTakesNewWork(t *testing.T, c *WSClient, f *terminalFixture, wantAgent int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wsTestDeadline)
	defer cancel()
	wf, err := c.Next(ctx, rpc.Filter{Labels: map[string]string{}})
	require.NoError(t, err)
	require.NotNil(t, wf, "agent must go on to take new work")
	assert.Equal(t, "117400", wf.ID)
	assert.Equal(t, wantAgent, <-f.polledWith, "the server must poll for the registered agent")
}

// A step.update the server rejects because the step is already terminal is a
// permanent answer: the agent must get it back typed, after one attempt, on a
// connection that still works.
func TestWSClient_TerminalRejection_IsDroppedNotRetried(t *testing.T) {
	f := newTerminalFixture(t)
	c, id := registeredClient(t, f)

	err := updateKilledStep(c)

	require.ErrorIs(t, err, rpc.ErrStepTerminal, "a terminal-state rejection must come back typed, not be retried")
	assert.Equal(t, int64(1), f.stepReads.Load(), "a permanent rejection is sent once")
	assertTakesNewWork(t, c, f, id)
}

// A dropped connection must come back REGISTERED, under the same agent id.
// Before #394 the client reconnected without re-registering, so the server's
// new connection had agent id 0, every update failed `sql: no rows`, and
// agent.next was refused "agent not registered", forever.
func TestWSClient_Reconnect_ReregistersUnderSameID(t *testing.T) {
	origMin := wsReconnectMin
	wsReconnectMin = 10 * time.Millisecond
	t.Cleanup(func() { wsReconnectMin = origMin })

	f := newTerminalFixture(t)
	c, id := registeredClient(t, f)

	c.mu.Lock()
	_ = c.conn.Close() // the connection drops under the agent
	c.mu.Unlock()
	require.Eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.conn == nil
	}, wsTestDeadline, 5*time.Millisecond)

	err := updateKilledStep(c)

	require.ErrorIs(t, err, rpc.ErrStepTerminal, "after a reconnect the server must know the agent (no `sql: no rows`)")
	assert.Equal(t, int64(1), f.creates.Load(), "a reconnect re-binds the same agent row, it does not mint a new one")
	assertTakesNewWork(t, c, f, id)
}
