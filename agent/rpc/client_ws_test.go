package rpc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"

	"go.woodpecker-ci.org/woodpecker/v3/rpc"
)

func mockWSServer(t *testing.T, handler func(*websocket.Conn)) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != "test-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		handler(conn)
	}))
}

func TestWSClient_BuildURL(t *testing.T) {
	c := &WSClient{
		serverURL: "d3ci42.peregrinetechsys.net:443",
		token:     "secret123",
		hostname:  "agent-1",
		secure:    true,
	}
	url := c.buildURL()
	assert.Contains(t, url, "wss://")
	assert.Contains(t, url, "/ws/agent")
	assert.Contains(t, url, "token=secret123")
	assert.Contains(t, url, "hostname=agent-1")
}

func TestWSClient_BuildURL_Insecure(t *testing.T) {
	c := &WSClient{
		serverURL: "localhost:8000",
		token:     "secret",
		hostname:  "agent-1",
		secure:    false,
	}
	url := c.buildURL()
	assert.Contains(t, url, "ws://localhost:8000/ws/agent")
}

func TestWSClient_BuildURL_StripsDNSPrefix(t *testing.T) {
	c := &WSClient{
		serverURL: "dns:///d3ci42.peregrinetechsys.net:443",
		token:     "secret",
		hostname:  "agent-1",
		secure:    true,
	}
	url := c.buildURL()
	assert.Contains(t, url, "wss://d3ci42.peregrinetechsys.net:443/ws/agent")
}

func TestWSClient_Wait_ReturnsOnCancel(t *testing.T) {
	// The critical behavioral test: Wait must return (false, nil) on context cancel,
	// NOT (false, error) which would kill the workflow (#3496/#3497).
	c := &WSClient{
		cancels: make(map[string]chan struct{}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	canceled, err := c.Wait(ctx, "wf-123")
	assert.False(t, canceled, "Wait should return canceled=false on context cancel")
	assert.NoError(t, err, "Wait must NOT return error on context cancel — this is the #3496 fix")
}

func TestWSClient_Wait_ReturnsOnServerCancel(t *testing.T) {
	c := &WSClient{
		cancels: make(map[string]chan struct{}),
	}

	// Simulate server sending task.cancel
	go func() {
		time.Sleep(50 * time.Millisecond)
		c.cancelsMu.Lock()
		if ch, ok := c.cancels["wf-123"]; ok {
			close(ch)
		}
		c.cancelsMu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	canceled, err := c.Wait(ctx, "wf-123")
	assert.True(t, canceled, "Wait should return canceled=true when server sends task.cancel")
	assert.NoError(t, err)
}

func TestWSClient_Version(t *testing.T) {
	srv := mockWSServer(t, func(conn *websocket.Conn) {
		// Send version message
		env, _ := json.Marshal(envelope{
			Type:    "version",
			Payload: json.RawMessage(`{"server_version":"v3.13.0-pts.12"}`),
		})
		conn.WriteMessage(websocket.TextMessage, env)
		// Keep connection alive briefly
		time.Sleep(100 * time.Millisecond)
	})
	defer srv.Close()

	wsURL := srv.URL[7:] // strip "http://"
	c := NewWSClient(context.Background(), wsURL, "test-secret", "agent-1", false).(*WSClient)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	v, err := c.Version(ctx)
	assert.NoError(t, err)
	assert.Equal(t, "v3.13.0-pts.12", v.ServerVersion)
}

func TestWSClient_RegisterAgent(t *testing.T) {
	srv := mockWSServer(t, func(conn *websocket.Conn) {
		// Read version from inbox first
		_, msg, _ := conn.ReadMessage()
		var env envelope
		json.Unmarshal(msg, &env)

		// Respond with registered
		resp, _ := json.Marshal(envelope{
			Type:    "registered",
			Ref:     env.Ref,
			Payload: json.RawMessage(`{"agent_id":42}`),
		})
		conn.WriteMessage(websocket.TextMessage, resp)
	})
	defer srv.Close()

	wsURL := srv.URL[7:]
	c := NewWSClient(context.Background(), wsURL, "test-secret", "agent-1", false).(*WSClient)

	// Force connection
	c.connect(context.Background())

	// Drain version message from inbox
	select {
	case <-c.inbox:
	case <-time.After(time.Second):
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	agentID, err := c.RegisterAgent(ctx, rpc.AgentInfo{
		Platform: "linux/amd64",
		Backend:  "local",
		Capacity: 1,
		Version:  "v3.13.0-pts.12",
	})
	assert.NoError(t, err)
	assert.Equal(t, int64(42), agentID)
}
