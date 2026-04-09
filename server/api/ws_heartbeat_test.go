package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"go.woodpecker-ci.org/woodpecker/v3/server"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	queue_mocks "go.woodpecker-ci.org/woodpecker/v3/server/queue/mocks"
	store_mocks "go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
)

func setupWSHeartbeatServer(t *testing.T, mockQueue *queue_mocks.MockQueue, mockStore *store_mocks.MockStore) *httptest.Server {
	t.Helper()
	gin.SetMode(gin.TestMode)

	server.Config.Server.AgentToken = "test-secret"
	if mockQueue != nil {
		server.Config.Services.Queue = mockQueue
	}

	r := gin.New()
	r.Use(func(c *gin.Context) {
		if mockStore != nil {
			c.Set("store", mockStore)
		}
		c.Next()
	})
	r.GET("/ws/heartbeat", WSHeartbeat)

	return httptest.NewServer(r)
}

func wsConnect(t *testing.T, srv *httptest.Server, token string) *websocket.Conn {
	t.Helper()
	url := "ws" + srv.URL[4:] + "/ws/heartbeat?token=" + token
	conn, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		if resp != nil {
			t.Fatalf("ws dial failed: %v (status %d)", err, resp.StatusCode)
		}
		t.Fatalf("ws dial failed: %v", err)
	}
	return conn
}

func TestWSHeartbeat_AuthRejectsInvalidToken(t *testing.T) {
	mockQueue := queue_mocks.NewMockQueue(t)
	srv := setupWSHeartbeatServer(t, mockQueue, nil)
	defer srv.Close()

	url := srv.URL + "/ws/heartbeat?token=wrong"
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestWSHeartbeat_AuthRejectsEmptyToken(t *testing.T) {
	mockQueue := queue_mocks.NewMockQueue(t)
	srv := setupWSHeartbeatServer(t, mockQueue, nil)
	defer srv.Close()

	url := srv.URL + "/ws/heartbeat"
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestWSHeartbeat_ExtendsWorkflowLeases(t *testing.T) {
	mockQueue := queue_mocks.NewMockQueue(t)
	mockStore := store_mocks.NewMockStore(t)

	mockQueue.On("Extend", mock.Anything, int64(42), "wf-100").Return(nil)
	mockQueue.On("Extend", mock.Anything, int64(42), "wf-200").Return(nil)

	mockStore.On("AgentFind", int64(42)).Return(&model.Agent{
		ID: 42, Name: "agent-1", LastContact: time.Now().Unix() - 30,
	}, nil)
	mockStore.On("AgentUpdate", mock.AnythingOfType("*model.Agent")).Return(nil)

	srv := setupWSHeartbeatServer(t, mockQueue, mockStore)
	defer srv.Close()

	conn := wsConnect(t, srv, "test-secret")
	defer conn.Close()

	msg, _ := json.Marshal(HeartbeatMessage{
		Hostname:    "agent-1",
		AgentID:     42,
		WorkflowIDs: []string{"wf-100", "wf-200"},
	})
	err := conn.WriteMessage(websocket.TextMessage, msg)
	assert.NoError(t, err)

	_, ackData, err := conn.ReadMessage()
	assert.NoError(t, err)

	var ack HeartbeatAck
	assert.NoError(t, json.Unmarshal(ackData, &ack))
	assert.True(t, ack.OK)
	assert.Equal(t, 2, ack.Extended)

	mockQueue.AssertExpectations(t)
}

func TestWSHeartbeat_RejectsMissingAgentID(t *testing.T) {
	mockQueue := queue_mocks.NewMockQueue(t)

	srv := setupWSHeartbeatServer(t, mockQueue, nil)
	defer srv.Close()

	conn := wsConnect(t, srv, "test-secret")
	defer conn.Close()

	msg, _ := json.Marshal(HeartbeatMessage{
		Hostname:    "agent-1",
		WorkflowIDs: []string{"wf-100"},
	})
	err := conn.WriteMessage(websocket.TextMessage, msg)
	assert.NoError(t, err)

	_, ackData, err := conn.ReadMessage()
	assert.NoError(t, err)

	var ack HeartbeatAck
	assert.NoError(t, json.Unmarshal(ackData, &ack))
	assert.False(t, ack.OK)
	assert.Equal(t, "missing agent_id", ack.Error)
}

func TestWSHeartbeat_EmptyWorkflowIDs_StillAcks(t *testing.T) {
	mockQueue := queue_mocks.NewMockQueue(t)
	mockStore := store_mocks.NewMockStore(t)

	mockStore.On("AgentFind", int64(42)).Return(&model.Agent{
		ID: 42, Name: "agent-1",
	}, nil)
	mockStore.On("AgentUpdate", mock.AnythingOfType("*model.Agent")).Return(nil)

	srv := setupWSHeartbeatServer(t, mockQueue, mockStore)
	defer srv.Close()

	conn := wsConnect(t, srv, "test-secret")
	defer conn.Close()

	msg, _ := json.Marshal(HeartbeatMessage{
		Hostname:    "agent-1",
		AgentID:     42,
		WorkflowIDs: nil,
	})
	err := conn.WriteMessage(websocket.TextMessage, msg)
	assert.NoError(t, err)

	_, ackData, err := conn.ReadMessage()
	assert.NoError(t, err)

	var ack HeartbeatAck
	assert.NoError(t, json.Unmarshal(ackData, &ack))
	assert.True(t, ack.OK)
	assert.Equal(t, 0, ack.Extended)
}
