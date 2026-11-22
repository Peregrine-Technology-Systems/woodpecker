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
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"go.woodpecker-ci.org/woodpecker/v3/server"
	"go.woodpecker-ci.org/woodpecker/v3/server/queue"
	queue_mocks "go.woodpecker-ci.org/woodpecker/v3/server/queue/mocks"
	grpcserver "go.woodpecker-ci.org/woodpecker/v3/server/rpc"
	store_mocks "go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
)

// #404 defect 1: the read loop handles each message inline, and its read
// deadline was renewed only by the pong handler, which runs only while the loop
// is reading. A store write slower than the pong wait therefore let the
// deadline lapse mid-handling, and the next read timed out and closed a
// healthy agent's socket. Here the log write stalls for twice the pong wait
// while the client keeps answering pings; the connection must survive.
func TestWSAgent_SlowStoreWriteDoesNotCloseALiveAgent(t *testing.T) {
	oldPing, oldPong := wsPingInterval, wsPongWait
	wsPingInterval, wsPongWait = 50*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { wsPingInterval, wsPongWait = oldPing, oldPong })

	gin.SetMode(gin.TestMode)
	server.Config.Server.AgentToken = "test-secret"
	q := queue_mocks.NewMockQueue(t)
	q.On("Info", mock.Anything).Return(queue.InfoT{}).Maybe()
	s := store_mocks.NewMockStore(t)
	stall := 2 * wsPongWait
	s.On("StepByUUID", mock.Anything).Run(func(mock.Arguments) { time.Sleep(stall) }).
		Return(nil, assert.AnError).Once()
	server.Config.Services.WSAgentRPC = grpcserver.NewRPCForTesting(q, s)
	t.Cleanup(func() { server.Config.Services.WSAgentRPC = nil })

	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("store", s); c.Next() })
	// Wait for the handler to exit before the cleanups above restore the
	// globals it reads.
	handlerDone := make(chan struct{})
	r.GET("/ws/agent", func(c *gin.Context) { defer close(handlerDone); WSAgent(c) })
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	conn, _, err := websocket.DefaultDialer.Dial("ws"+srv.URL[4:]+"/ws/agent?token=test-secret&hostname=busy-agent", nil)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close(); <-handlerDone })

	// The client reads continuously, as the agent's readPump does, so it
	// answers every ping; frames go to a channel for the assertions below.
	frames := make(chan []byte, 64)
	closed := make(chan error, 1)
	go func() {
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				closed <- err
				return
			}
			frames <- msg
		}
	}()

	logMsg, err := newEnvelope(MsgStepLog, "", LogPayload{StepUUID: "step-1", Entries: []LogEntryWS{{Line: 1}}})
	require.NoError(t, err)
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, logMsg))

	// Outlast the stall and then one more pong wait.
	select {
	case err := <-closed:
		t.Fatalf("the server closed a live agent's socket during a slow store write: %v", err)
	case <-time.After(stall + wsPongWait):
	}

	// Still serving: an unknown message type is answered with an ack.
	probe, err := newEnvelope("probe", "r1", struct{}{})
	require.NoError(t, err)
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, probe))
	deadline := time.After(2 * time.Second)
	for {
		select {
		case msg := <-frames:
			if strings.Contains(string(msg), `"ref":"r1"`) {
				return
			}
		case err := <-closed:
			t.Fatalf("connection closed before answering: %v", err)
		case <-deadline:
			t.Fatal("no answer to the probe")
		}
	}
}
