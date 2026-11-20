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
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.woodpecker-ci.org/woodpecker/v3/rpc"
)

// #394: an error the server ANSWERED (an ack with ok=false) arrived on a
// working connection. Re-dialling cannot change it, and retrying it forever
// keeps a useless agent looking healthy. It is retried a bounded number of
// times on the same connection, then returned.
func TestWSClient_ServerRejection_IsBoundedAndKeepsConnection(t *testing.T) {
	var conns, requests atomic.Int64
	srv := mockWSServer(t, func(conn *websocket.Conn) {
		conns.Add(1)
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var env envelope
			_ = json.Unmarshal(msg, &env)
			if env.Type == "agent.next" {
				reply, _ := json.Marshal(envelope{Type: "task.assign", Ref: env.Ref,
					Payload: json.RawMessage(`{"id":"117400","timeout":60,"config":{}}`)})
				_ = conn.WriteMessage(websocket.TextMessage, reply)
				continue
			}
			requests.Add(1)
			payload, _ := json.Marshal(map[string]any{"ok": false, "error": "sql: no rows in result set"})
			reply, _ := json.Marshal(envelope{Type: "ack", Ref: env.Ref, Payload: payload})
			_ = conn.WriteMessage(websocket.TextMessage, reply)
		}
	})
	defer srv.Close()

	c := NewWSClient(context.Background(), srv.URL[len("http://"):], "test-secret", "h", false).(*WSClient)
	ctx, cancel := context.WithTimeout(context.Background(), wsTestDeadline)
	defer cancel()

	start := time.Now()
	err := c.Done(ctx, "117374", rpc.WorkflowState{Finished: 1})

	require.Error(t, err)
	assert.NoError(t, ctx.Err(), "must give up on its own, not at the caller's deadline")
	assert.Contains(t, err.Error(), "sql: no rows")
	assert.Equal(t, int64(wsMaxServerRejections), requests.Load())
	assert.Equal(t, int64(1), conns.Load(), "a server-answered error must not tear down the connection")
	assert.Less(t, time.Since(start), wsTestDeadline)

	// Giving up on one request is not giving up on the agent: on the same
	// connection it goes straight back to taking work (ENG's question on #396).
	wf, err := c.Next(ctx, rpc.Filter{Labels: map[string]string{}})
	require.NoError(t, err)
	require.NotNil(t, wf, "after a bounded rejection the agent must take new work, not sit idle")
	assert.Equal(t, "117400", wf.ID)
	assert.Equal(t, int64(1), conns.Load())
}
