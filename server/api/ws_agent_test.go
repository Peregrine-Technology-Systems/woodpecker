package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"

	"go.woodpecker-ci.org/woodpecker/v3/server"
)

func setupWSAgentServer(t *testing.T) *httptest.Server {
	t.Helper()
	gin.SetMode(gin.TestMode)
	server.Config.Server.AgentToken = "test-secret"

	r := gin.New()
	r.GET("/ws/agent", WSAgent)
	return httptest.NewServer(r)
}

func wsAgentConnect(t *testing.T, srv *httptest.Server, token, hostname string) *websocket.Conn {
	t.Helper()
	url := "ws" + srv.URL[4:] + "/ws/agent?token=" + token + "&hostname=" + hostname
	conn, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		if resp != nil {
			t.Fatalf("ws dial failed: %v (status %d)", err, resp.StatusCode)
		}
		t.Fatalf("ws dial failed: %v", err)
	}
	return conn
}

func TestWSAgent_AuthRejectsInvalidToken(t *testing.T) {
	srv := setupWSAgentServer(t)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/ws/agent?token=wrong")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestWSAgent_AuthRejectsEmptyToken(t *testing.T) {
	srv := setupWSAgentServer(t)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/ws/agent")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestWSAgent_ProtocolPayloadRoundTrip(t *testing.T) {
	// Test that payloads survive JSON round-trip correctly
	original := TaskAssignPayload{
		ID:      "wf-123",
		Timeout: 3600,
		Config:  json.RawMessage(`{"stages":[]}`),
	}
	data, err := newEnvelope(MsgTaskAssign, "ref-1", original)
	assert.NoError(t, err)

	var env Envelope
	assert.NoError(t, json.Unmarshal(data, &env))

	var decoded TaskAssignPayload
	assert.NoError(t, json.Unmarshal(env.Payload, &decoded))
	assert.Equal(t, "wf-123", decoded.ID)
	assert.Equal(t, int64(3600), decoded.Timeout)
}

func TestWSAgent_ProtocolEnvelopeMarshal(t *testing.T) {
	data, err := newEnvelope(MsgAck, "ref-1", AckPayload{OK: true})
	assert.NoError(t, err)

	var env Envelope
	assert.NoError(t, json.Unmarshal(data, &env))
	assert.Equal(t, MsgAck, env.Type)
	assert.Equal(t, "ref-1", env.Ref)

	var ack AckPayload
	assert.NoError(t, json.Unmarshal(env.Payload, &ack))
	assert.True(t, ack.OK)
}

func TestWSAgent_ProtocolAllMessageTypes(t *testing.T) {
	// Verify all message type constants are unique
	types := map[string]bool{}
	for _, mt := range []string{
		MsgAgentRegister, MsgAgentUnregister, MsgAgentNext,
		MsgHealth, MsgExtend, MsgWorkflowInit, MsgWorkflowDone,
		MsgStepUpdate, MsgStepLog,
		MsgVersion, MsgRegistered, MsgTaskAssign, MsgTaskCancel, MsgAck,
	} {
		assert.False(t, types[mt], "duplicate message type: %s", mt)
		types[mt] = true
	}
	assert.Len(t, types, 14)
}
