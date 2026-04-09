package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
)

func TestWSHeartbeatClient_SendsHeartbeats(t *testing.T) {
	received := make(chan wsHeartbeatMsg, 10)

	// Mock WS server
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != "test-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var hb wsHeartbeatMsg
			json.Unmarshal(msg, &hb)
			received <- hb

			ack, _ := json.Marshal(wsHeartbeatAck{OK: true, Extended: len(hb.WorkflowIDs)})
			conn.WriteMessage(websocket.TextMessage, ack)
		}
	}))
	defer srv.Close()

	state := &State{Metadata: map[string]Info{
		"wf-100": {ID: "wf-100", Repo: "org/repo"},
		"wf-200": {ID: "wf-200", Repo: "org/repo2"},
	}}

	// Build WS URL from httptest server
	wsURL := "ws" + srv.URL[4:] // strip "http", add "ws"

	client := &WSHeartbeatClient{
		serverURL: wsURL[5:], // strip "ws://"
		token:     "test-secret",
		agentID:   42,
		hostname:  "agent-1",
		state:     state,
		secure:    false,
	}

	// Override buildURL for test (use test server URL directly)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Run in background — will send heartbeats until ctx canceled
	go func() {
		// Connect directly to test server
		conn, _, err := websocket.DefaultDialer.DialContext(ctx,
			wsURL+"/ws/heartbeat?token=test-secret", nil)
		if err != nil {
			return
		}
		defer conn.Close()

		msg, _ := json.Marshal(wsHeartbeatMsg{
			Hostname:    client.hostname,
			AgentID:     client.agentID,
			WorkflowIDs: state.RunningIDs(),
		})
		conn.WriteMessage(websocket.TextMessage, msg)

		// Read ack
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		conn.ReadMessage()
	}()

	// Wait for heartbeat
	select {
	case hb := <-received:
		assert.Equal(t, "agent-1", hb.Hostname)
		assert.Equal(t, int64(42), hb.AgentID)
		assert.Len(t, hb.WorkflowIDs, 2)
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for heartbeat")
	}
}

func TestWSHeartbeatClient_BuildURL(t *testing.T) {
	client := &WSHeartbeatClient{
		serverURL: "d3ci42.peregrinetechsys.net:443",
		token:     "secret123",
		secure:    true,
	}
	url := client.buildURL()
	assert.Contains(t, url, "wss://")
	assert.Contains(t, url, "/ws/heartbeat")
	assert.Contains(t, url, "token=secret123")

	client.secure = false
	url = client.buildURL()
	assert.Contains(t, url, "ws://")
}

func TestWSHeartbeatClient_BuildURL_StripsDNSPrefix(t *testing.T) {
	client := &WSHeartbeatClient{
		serverURL: "dns:///d3ci42.peregrinetechsys.net:443",
		token:     "secret",
		secure:    true,
	}
	url := client.buildURL()
	assert.Contains(t, url, "wss://d3ci42.peregrinetechsys.net:443/ws/heartbeat")
}

func TestRunningIDs(t *testing.T) {
	state := &State{Metadata: map[string]Info{
		"wf-1": {ID: "wf-1"},
		"wf-2": {ID: "wf-2"},
	}}
	ids := state.RunningIDs()
	assert.Len(t, ids, 2)
	assert.Contains(t, ids, "wf-1")
	assert.Contains(t, ids, "wf-2")
}

func TestRunningIDs_Empty(t *testing.T) {
	state := &State{Metadata: map[string]Info{}}
	ids := state.RunningIDs()
	assert.Empty(t, ids)
}
