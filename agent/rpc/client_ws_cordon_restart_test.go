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
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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

// #440, the broken state end to end: the REAL agent WSClient, the REAL WSAgent
// handler and the REAL PatchAgent. An agent registers, picks up a task, and is
// cordoned the way the scaler does it (PATCH {"no_schedule": true}). Then the
// connection drops, as on a server restart. The agents table persists across
// that, as the database does. The agent must come back under its old id, or
// its running task is orphaned on a "dead" agent.

type agentTable struct {
	mu     sync.Mutex
	rows   map[int64]model.Agent
	nextID int64
}

func (a *agentTable) find(id int64) (*model.Agent, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	row, ok := a.rows[id]
	if !ok {
		return nil, types.ErrRecordNotExist
	}
	return &row, nil
}

func TestWSClient_CordonedAgentKeepsItsIDAcrossARestart(t *testing.T) {
	origMin := wsReconnectMin
	wsReconnectMin = 10 * time.Millisecond
	t.Cleanup(func() { wsReconnectMin = origMin })
	gin.SetMode(gin.TestMode)
	server.Config.Server.AgentToken = "test-secret"
	table := &agentTable{rows: map[int64]model.Agent{}, nextID: 64080}
	var creates atomic.Int64

	s := store_mocks.NewMockStore(t)
	s.On("AgentCreate", mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		a := args.Get(0).(*model.Agent)
		table.mu.Lock()
		a.ID = table.nextID
		table.nextID++
		table.rows[a.ID] = *a
		table.mu.Unlock()
		creates.Add(1)
	}).Maybe()
	s.On("AgentFind", mock.AnythingOfType("int64")).Return(table.find).Maybe()
	s.On("AgentUpdate", mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		a := args.Get(0).(*model.Agent)
		table.mu.Lock()
		table.rows[a.ID] = *a
		table.mu.Unlock()
	}).Maybe()
	s.On("AgentList", mock.Anything).Return([]*model.Agent{}, nil).Maybe()
	s.On("WorkflowLoad", mock.Anything).Return(&model.Workflow{ID: 129898, PipelineID: 100}, nil).Maybe()
	s.On("WorkflowUpdate", mock.Anything).Return(nil).Maybe()
	s.On("GetPipeline", mock.Anything).Return(&model.Pipeline{ID: 100, RepoID: 9, Status: model.StatusRunning}, nil).Maybe()
	s.On("GetRepo", mock.Anything).Return(&model.Repo{ID: 9, FullName: "org/infra"}, nil).Maybe()

	q := queue_mocks.NewMockQueue(t)
	polled := make(chan int64, 4)
	data, _ := json.Marshal(rpc.Workflow{ID: "129898"})
	q.On("Poll", mock.Anything, mock.AnythingOfType("int64"), mock.Anything).Return(
		func(_ context.Context, agentID int64, _ queue.FilterFn) (*model.Task, error) {
			polled <- agentID
			return &model.Task{ID: "129898", Data: data}, nil
		}).Maybe()
	q.On("KickAgentWorkers", mock.Anything).Maybe()
	q.On("Wait", mock.Anything, mock.Anything).Return(nil).Maybe()
	q.On("Info", mock.Anything).Return(queue.InfoT{}).Maybe()
	prevQ := server.Config.Services.Queue
	server.Config.Services.Queue = q
	server.Config.Services.WSAgentRPC = grpcserver.NewRPCForTesting(q, s)
	t.Cleanup(func() { server.Config.Services.Queue = prevQ; server.Config.Services.WSAgentRPC = nil })

	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("store", s); c.Next() })
	r.GET("/ws/agent", api.WSAgent)
	r.PATCH("/api/agents/:agent_id", api.PatchAgent)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), wsTestDeadline)
	defer cancel()
	c := NewWSClient(context.Background(), srv.URL[len("http://"):], "test-secret", "ci-spot-host", false).(*WSClient)
	id, err := c.RegisterAgent(ctx, rpc.AgentInfo{Capacity: 1, Backend: "local", Platform: "linux/amd64"})
	require.NoError(t, err)
	wf, err := c.Next(ctx, rpc.Filter{Labels: map[string]string{}})
	require.NoError(t, err)
	require.NotNil(t, wf, "the agent holds a task")
	require.Equal(t, id, <-polled)

	// The scaler's cordon, byte for byte its request body.
	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/api/agents/"+strconv.FormatInt(id, 10), strings.NewReader(`{"no_schedule":true}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// The restart: every connection drops.
	c.mu.Lock()
	_ = c.conn.Close()
	c.mu.Unlock()
	require.Eventually(t, func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.conn == nil }, wsTestDeadline, 5*time.Millisecond)

	// The next call reconnects and re-registers. A cordoned agent is given no
	// new work, so it is asked for none here.
	_, err = c.Next(ctx, rpc.Filter{Labels: map[string]string{}})
	require.NoError(t, err)

	c.mu.Lock()
	got := c.agentID
	c.mu.Unlock()
	assert.Equal(t, id, got, "the cordoned agent must come back under its old id")
	select {
	case other := <-polled:
		t.Errorf("work was polled for agent %d after the reconnect: a fresh, uncordoned id", other)
	default:
	}
	assert.Equal(t, int64(1), creates.Load(), "no fresh agent id is minted")
	row, err := table.find(id)
	require.NoError(t, err)
	assert.Equal(t, "ci-spot-host", row.Name)
	assert.True(t, row.NoSchedule, "the cordon survives the reconnect")
}
