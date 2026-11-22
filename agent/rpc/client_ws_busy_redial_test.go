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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.woodpecker-ci.org/woodpecker/v3/rpc"
)

// #404: a busy agent (running a long step, so no Next and no step.update in
// flight) whose socket the server drops must get back on its own, through the
// calls it keeps making while busy: the 10 s health report and the lease
// extend. They used a bare send, which only returns "not connected", so the
// agent stayed off until the step ended and the server's 30 s grace killed the
// work.

// dropFirstServer registers the agent on every connection, closes the first
// one right after registering (the server-side drop), and records which
// message types arrived on which connection.
type dropFirstServer struct {
	conns    atomic.Int64
	mu       sync.Mutex
	got      map[int64][]string
	priorIDs []int64
	dropped  chan struct{}
}

func (s *dropFirstServer) seen(conn int64, msgType string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.got[conn] {
		if t == msgType {
			return true
		}
	}
	return false
}

func newDropFirstServer(t *testing.T) (*dropFirstServer, *httptest.Server) {
	t.Helper()
	s := &dropFirstServer{got: map[int64][]string{}, dropped: make(chan struct{})}
	hold := holdUntilCleanup(t)
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		n := s.conns.Add(1)
		go func() { <-hold; conn.Close() }()
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var env envelope
			_ = json.Unmarshal(msg, &env)
			s.mu.Lock()
			s.got[n] = append(s.got[n], env.Type)
			s.mu.Unlock()
			if env.Type != "agent.register" {
				continue
			}
			var p struct {
				AgentID int64 `json:"agent_id"`
			}
			_ = json.Unmarshal(env.Payload, &p)
			s.mu.Lock()
			s.priorIDs = append(s.priorIDs, p.AgentID)
			s.mu.Unlock()
			resp, _ := json.Marshal(envelope{Type: "registered", Ref: env.Ref, Payload: json.RawMessage(`{"agent_id":7}`)})
			_ = conn.WriteMessage(websocket.TextMessage, resp)
			if n == 1 {
				close(s.dropped)
				return // the server-side drop
			}
		}
	}))
	t.Cleanup(srv.Close)
	return s, srv
}

// busyAgentAfterDrop registers an agent, then waits until the server has dropped
// it and the client has noticed: from here on the agent is "busy" and nothing
// but health and extend will touch the connection.
func busyAgentAfterDrop(t *testing.T) (*dropFirstServer, *WSClient) {
	t.Helper()
	// The drop comes milliseconds after connecting, which ensureConnected treats
	// as a short-lived connection and backs off for; shrink that to keep the test fast.
	oldMin, oldMax := wsReconnectMin, wsReconnectMax
	wsReconnectMin, wsReconnectMax = 5*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { wsReconnectMin, wsReconnectMax = oldMin, oldMax })
	s, srv := newDropFirstServer(t)
	c := NewWSClient(context.Background(), srv.URL[7:], "test-secret", "agent-1", false).(*WSClient)
	ctx, cancel := context.WithTimeout(context.Background(), wsTestDeadline)
	defer cancel()
	id, err := c.RegisterAgent(ctx, rpc.AgentInfo{Platform: "linux/amd64", Capacity: 1})
	require.NoError(t, err)
	require.Equal(t, int64(7), id)
	<-s.dropped
	require.Eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.conn == nil
	}, wsTestDeadline, 5*time.Millisecond, "precondition: the client sees the drop")
	return s, c
}

func TestWSClient_BusyAgentHealthReportRedialsAfterServerDrop(t *testing.T) {
	s, c := busyAgentAfterDrop(t)
	ctx, cancel := context.WithTimeout(context.Background(), wsTestDeadline)
	defer cancel()

	require.NoError(t, c.ReportHealth(ctx), "a health report after a drop must reconnect, not fail with not connected")

	assert.Equal(t, int64(2), s.conns.Load(), "the agent dialled again")
	assert.Equal(t, []int64{0, 7}, s.priorIDs, "the new connection re-registers as the same agent")
	assert.Eventually(t, func() bool { return s.seen(2, "health") }, wsTestDeadline, 5*time.Millisecond,
		"the health report, which carries the running workflow ids, reached the server")
}

func TestWSClient_BusyAgentExtendRedialsAfterServerDrop(t *testing.T) {
	s, c := busyAgentAfterDrop(t)
	ctx, cancel := context.WithTimeout(context.Background(), wsTestDeadline)
	defer cancel()

	require.NoError(t, c.Extend(ctx, "wf-1"), "a lease extend after a drop must reconnect, not fail with not connected")

	assert.Equal(t, int64(2), s.conns.Load())
	assert.Eventually(t, func() bool { return s.seen(2, "extend") }, wsTestDeadline, 5*time.Millisecond)
}

// Two busy-path callers noticing the same drop must not dial twice: a second
// connect closes the first's fresh socket, which the server sees as one more drop.
func TestWSClient_ConcurrentRedialsDialOnce(t *testing.T) {
	s, c := busyAgentAfterDrop(t)
	ctx, cancel := context.WithTimeout(context.Background(), wsTestDeadline)
	defer cancel()

	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() { assert.NoError(t, c.ReportHealth(ctx)) })
	}
	wg.Wait()

	assert.Equal(t, int64(2), s.conns.Load(), "one redial for one drop")
}
