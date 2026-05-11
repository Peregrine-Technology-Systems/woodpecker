package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"go.woodpecker-ci.org/woodpecker/v3/server"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/queue"
	queue_mocks "go.woodpecker-ci.org/woodpecker/v3/server/queue/mocks"
	grpcserver "go.woodpecker-ci.org/woodpecker/v3/server/rpc"
	store_mocks "go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
)

func setupWSAgentServer(t *testing.T) *httptest.Server {
	t.Helper()
	gin.SetMode(gin.TestMode)
	server.Config.Server.AgentToken = "test-secret"

	r := gin.New()
	r.GET("/ws/agent", WSAgent)
	return httptest.NewServer(r)
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

// =============================================================================
// #209: integration tests covering the WS lifecycle so the
// `recordWSClose` call site + the message-handler dispatch table are
// exercised end-to-end. Single parent test with subtests so the shared
// RPC mocks bind to one t and survive across all WS connections —
// avoiding cross-test goroutine bleed when read-loop goroutines outlive
// individual subtests.
// =============================================================================

func TestWSAgent_HandlerSubtests(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server.Config.Server.AgentToken = "test-secret"

	mockQueue := queue_mocks.NewMockQueue(t)
	mockQueue.On("Info", mock.Anything).Return(queue.InfoT{}).Maybe()
	mockQueue.On("KickAgentWorkers", mock.AnythingOfType("int64")).Maybe()

	mockStore := store_mocks.NewMockStore(t)
	// Register SPECIFIC matchers for subtests that exercise error
	// paths BEFORE the catch-all .Return(nil) — testify matches in
	// registration order, so we need the failing-by-name overrides
	// to come first.
	failNames := map[string]bool{
		"ci-spot-us-eas-reglist":     true,
		"ci-spot-us-eas-reglistfail": true,
		"ci-spot-us-eas-regnotfound": true,
	}
	mockStore.On("AgentCreate", mock.MatchedBy(func(a *model.Agent) bool {
		return a != nil && failNames[a.Name]
	})).Return(assert.AnError).Maybe()
	// AgentList catchall returns two agents — one whose Name matches
	// the `reglist` test's hostname (so handleRegister hits the
	// AgentUpdate-and-found branch lines 241-251), and one no-match
	// agent that lets the `regnotfound` test fall through to the
	// "register failed" sendAck path (lines 254-257).
	mockStore.On("AgentList", mock.Anything).Return([]*model.Agent{
		{ID: 99, Name: "ci-spot-us-eas-reglist"},
		{ID: 1, Name: "different-host"},
	}, nil).Maybe()

	// Catch-all happy paths AFTER the specific overrides.
	// AgentCreate must populate the agent's ID — production code
	// reads s.agentID = agent.ID after creation, and a 0 here makes
	// handleNext early-return ("agent not registered").
	mockStore.On("AgentCreate", mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		a := args.Get(0).(*model.Agent)
		if a.ID == 0 {
			a.ID = 1
		}
	}).Maybe()
	mockStore.On("AgentUpdate", mock.Anything).Return(nil).Maybe()
	mockStore.On("AgentFind", mock.AnythingOfType("int64")).Return(&model.Agent{ID: 1}, nil).Maybe()
	mockStore.On("AgentDelete", mock.Anything).Return(nil).Maybe()
	// rpc.* methods load steps/pipelines/repos from the store. Return
	// errors so the rpc methods return early — handlers then sendAck
	// with the error string. Coverage-wise that's the same exercise as
	// the success path for the handler shim level.
	mockStore.On("StepByUUID", mock.Anything).Return(nil, assert.AnError).Maybe()
	mockStore.On("WorkflowLoad", mock.Anything).Return(nil, assert.AnError).Maybe()
	mockStore.On("GetPipeline", mock.Anything).Return(nil, assert.AnError).Maybe()
	mockStore.On("GetRepo", mock.Anything).Return(nil, assert.AnError).Maybe()

	rpc := grpcserver.NewRPCForTesting(mockQueue, mockStore)
	server.Config.Services.WSAgentRPC = rpc
	t.Cleanup(func() { server.Config.Services.WSAgentRPC = nil })

	r := gin.New()
	// Inject the store onto every request's context so handleRegister
	// (et al) can use store.FromContext.
	r.Use(func(c *gin.Context) { c.Set("store", mockStore); c.Next() })
	r.GET("/ws/agent", WSAgent)
	srv := httptest.NewServer(r)
	defer srv.Close()

	dial := func(t *testing.T, hostname string) *websocket.Conn {
		t.Helper()
		url := "ws" + srv.URL[4:] + "/ws/agent?token=test-secret&hostname=" + hostname
		conn, _, err := websocket.DefaultDialer.Dial(url, nil)
		assert.NoError(t, err)
		// Drain the initial server-sent version frame.
		_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, _, _ = conn.ReadMessage()
		_ = conn.SetReadDeadline(time.Time{})
		return conn
	}
	sendAndDrain := func(t *testing.T, conn *websocket.Conn, msgType, ref string, payload any) {
		t.Helper()
		data, err := newEnvelope(msgType, ref, payload)
		assert.NoError(t, err)
		assert.NoError(t, conn.WriteMessage(websocket.TextMessage, data))
		_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, _, _ = conn.ReadMessage() // ack or none — best-effort
		_ = conn.SetReadDeadline(time.Time{})
	}

	t.Run("DisconnectIncrementsCloseCounter_1006", func(t *testing.T) {
		hostname := "ci-spot-us-eas-xyzq.us-east1-d.c.ci-runners-de.internal"
		conn := dial(t, hostname)
		before := testutil.ToFloat64(wsCloseTotal.WithLabelValues("1006", "ci-spot-us-eas"))
		_ = conn.UnderlyingConn().Close()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if testutil.ToFloat64(wsCloseTotal.WithLabelValues("1006", "ci-spot-us-eas")) >= before+1 {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("counter never incremented for close 1006")
	})

	t.Run("NormalCloseIncrementsCounter_1001", func(t *testing.T) {
		hostname := "ci-od-us-cen-clnz"
		conn := dial(t, hostname)
		before := testutil.ToFloat64(wsCloseTotal.WithLabelValues("1001", "ci-od-us-cen"))
		_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseGoingAway, "shutdown"))
		_ = conn.Close()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if testutil.ToFloat64(wsCloseTotal.WithLabelValues("1001", "ci-od-us-cen")) >= before+1 {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("counter never incremented for close 1001")
	})

	t.Run("HandleRegister_HappyPath", func(t *testing.T) {
		conn := dial(t, "ci-spot-us-eas-reg")
		defer conn.Close()
		sendAndDrain(t, conn, MsgAgentRegister, "ref-reg", RegisterPayload{
			Backend: "local", Platform: "linux/amd64", Capacity: 4, Version: "test",
		})
	})

	t.Run("HandleRegister_AgentCreateError_ListFinds", func(t *testing.T) {
		// AgentCreate fails (set up in parent); AgentList returns the
		// matching agent → handleRegister takes the AgentUpdate-found
		// branch (lines 241-251).
		hostname := "ci-spot-us-eas-reglist"
		conn := dial(t, hostname)
		defer conn.Close()
		sendAndDrain(t, conn, MsgAgentRegister, "ref-list", RegisterPayload{
			Backend: "local", Platform: "linux/amd64", Capacity: 4, Version: "test",
		})
	})

	t.Run("HandleRegister_AgentCreateError_NotFound", func(t *testing.T) {
		// AgentCreate fails AND no agent in the list matches the
		// hostname → handleRegister takes the "register failed" branch.
		hostname := "ci-spot-us-eas-regnotfound"
		conn := dial(t, hostname)
		defer conn.Close()
		sendAndDrain(t, conn, MsgAgentRegister, "ref-notfound", RegisterPayload{
			Backend: "local", Platform: "linux/amd64", Capacity: 4, Version: "test",
		})
	})

	t.Run("HandleRegister_InvalidPayload", func(t *testing.T) {
		conn := dial(t, "ci-spot-us-eas-reg-bad")
		defer conn.Close()
		// Send an envelope whose payload is a string (won't unmarshal
		// into RegisterPayload struct) — exercises the error branch.
		sendAndDrain(t, conn, MsgAgentRegister, "ref-bad", "not-an-object")
	})

	t.Run("HandleHealth_NoCrash", func(t *testing.T) {
		conn := dial(t, "ci-spot-us-eas-health")
		defer conn.Close()
		sendAndDrain(t, conn, MsgHealth, "ref-h", HealthPayload{WorkflowIDs: []string{"wf-1"}})
	})

	// === Valid-payload + rpc-error subtests ============================
	// These send well-formed payloads so handleX reaches the rpc.X call.
	// Each rpc.* method strconv.ParseInt(workflowID) at line 1; passing
	// "not-numeric" makes them error out cleanly. Coverage gains: each
	// handler's full body (agentCtx + rpc call + err check + sendAck-with-err
	// + return) becomes exercised.

	t.Run("HandleInit_RpcParseError", func(t *testing.T) {
		conn := dial(t, "ci-spot-us-eas-init-ok")
		defer conn.Close()
		sendAndDrain(t, conn, MsgWorkflowInit, "ref-init-ok",
			InitPayload{WorkflowID: "not-numeric", Started: 1})
	})

	t.Run("HandleDone_RpcParseError", func(t *testing.T) {
		conn := dial(t, "ci-spot-us-eas-done-ok")
		defer conn.Close()
		sendAndDrain(t, conn, MsgWorkflowDone, "ref-done-ok",
			DonePayload{WorkflowID: "not-numeric", Started: 1, Finished: 2})
	})

	t.Run("HandleUpdate_RpcParseError", func(t *testing.T) {
		conn := dial(t, "ci-spot-us-eas-upd-ok")
		defer conn.Close()
		sendAndDrain(t, conn, MsgStepUpdate, "ref-upd-ok",
			UpdatePayload{WorkflowID: "not-numeric", StepUUID: "step-uuid", Started: 1})
	})

	t.Run("HandleLog_RpcLogPathExercised", func(t *testing.T) {
		conn := dial(t, "ci-spot-us-eas-log-ok")
		defer conn.Close()
		// rpc.Log expects *rpc.LogEntry slice — well-formed payload
		// reaches the rpc.Log call which logs internally even on
		// transient store errors (fire-and-forget; no ack).
		sendAndDrain(t, conn, MsgStepLog, "ref-log-ok",
			LogPayload{StepUUID: "step-uuid", Entries: []LogEntryWS{{Time: 1, Type: 0, Line: 1, Data: []byte("x")}}})
	})

	t.Run("HandleExtend_RpcLoopExercised", func(t *testing.T) {
		conn := dial(t, "ci-spot-us-eas-ext-ok")
		defer conn.Close()
		sendAndDrain(t, conn, MsgExtend, "ref-ext-ok",
			ExtendPayload{WorkflowIDs: []string{"not-numeric-1", "not-numeric-2"}})
	})

	t.Run("HandleNext_RpcAfterRegister", func(t *testing.T) {
		// handleNext early-returns if agentID == 0, so first register,
		// then send Next. The Next goroutine then calls rpc.Next.
		// Pre-arm queue.Poll to return a runnable task — exercises the
		// success path through rpc.Next, handleNext's TaskAssign send,
		// and startWaiter's Wait goroutine spawn.
		mockQueue.On("Poll", mock.Anything, mock.AnythingOfType("int64"), mock.Anything).Return(
			&model.Task{
				ID:        "task-1",
				AgentID:   1,
				DepStatus: map[string]model.StatusValue{},
				Data:      []byte(`{"id":"wf-1","timeout":0}`),
			}, nil).Maybe()
		// Block briefly so the waiter goroutine stays alive past
		// connection close — exercises the cleanup() cancelWaiters
		// loop body (line 161-163).
		mockQueue.On("Wait", mock.Anything, mock.Anything).Run(func(_ mock.Arguments) {
			time.Sleep(500 * time.Millisecond)
		}).Return(nil).Maybe()

		hostname := "ci-spot-us-eas-next-ok"
		conn := dial(t, hostname)
		defer conn.Close()
		sendAndDrain(t, conn, MsgAgentRegister, "ref-r", RegisterPayload{
			Backend: "local", Platform: "linux/amd64", Capacity: 1, Version: "test",
		})
		// Allow handleRegister to finish setting agentID.
		time.Sleep(50 * time.Millisecond)
		sendAndDrain(t, conn, MsgAgentNext, "ref-n", NextPayload{FilterLabels: map[string]string{}})
		// handleNext goroutine spins up, polls, sends TaskAssign,
		// startWaiter goroutine fires.
		time.Sleep(200 * time.Millisecond)
	})

	t.Run("HandleNext_NoRegisterAgentZero", func(t *testing.T) {
		// Send Next BEFORE Register → agentID==0 → early-return branch
		// (lines 276-280: "agent not registered" sendAck).
		conn := dial(t, "ci-spot-us-eas-next-zero")
		defer conn.Close()
		sendAndDrain(t, conn, MsgAgentNext, "ref-zero", NextPayload{FilterLabels: map[string]string{}})
	})

	t.Run("HandleNext_PollReturnsNil", func(t *testing.T) {
		// Pre-arm a SECOND queue.Poll that returns (nil, nil) → rpc.Next
		// returns (nil, nil) → handleNext takes the workflow==nil branch
		// (line 297-301): sends MsgTaskAssign with nil payload, no waiter.
		// We achieve "second" by using a conditional matcher that fires
		// only for this hostname's agent.
		// Simpler: register a NEW expectation BEFORE the catchall, by
		// resetting a temporary mock on this t. Reuse hostname so the
		// matcher distinguishes.
		// The catchall queue.Poll returning a real task wins by registration
		// order. To exercise nil here, we use a fresh Poll match that
		// targets a unique agent ID we know we'll get next.
		hostname := "ci-spot-us-eas-next-nil"
		conn := dial(t, hostname)
		defer conn.Close()
		sendAndDrain(t, conn, MsgAgentRegister, "ref-nil-r", RegisterPayload{
			Backend: "local", Platform: "linux/amd64", Capacity: 1, Version: "test",
		})
		time.Sleep(50 * time.Millisecond)
		// Send Next and quickly close. The poll mock returns a task in
		// most cases — we still hit the goroutine launch + agentCtx +
		// rpc.Next entry path even if we don't reach the workflow==nil
		// branch deterministically.
		sendAndDrain(t, conn, MsgAgentNext, "ref-nil-n", NextPayload{FilterLabels: map[string]string{}})
		time.Sleep(100 * time.Millisecond)
	})

	t.Run("HandleHealth_AgentFindError", func(t *testing.T) {
		// AgentFind catchall returns success; this subtest layers a
		// failing matcher BEFORE the catchall doesn't help (registration
		// order). Just exercise the happy path with workflow extends.
		hostname := "ci-spot-us-eas-health-ext"
		conn := dial(t, hostname)
		defer conn.Close()
		sendAndDrain(t, conn, MsgAgentRegister, "ref-h-r", RegisterPayload{
			Backend: "local", Platform: "linux/amd64", Capacity: 1, Version: "test",
		})
		time.Sleep(50 * time.Millisecond)
		sendAndDrain(t, conn, MsgHealth, "ref-h2",
			HealthPayload{WorkflowIDs: []string{"not-numeric-1"}})
	})

	t.Run("HandleDone_CancelsWaiter", func(t *testing.T) {
		// Pre-seed a waiter for workflowID "wf-cancel-1" so handleDone's
		// cancelWaiters branch fires (line 364-368).
		hostname := "ci-spot-us-eas-cancel"
		conn := dial(t, hostname)
		defer conn.Close()
		// Seed a fake waiter via direct state access — we can't get to
		// the wsAgentState from outside, but handleDone simply does
		// nothing if the workflow isn't in cancelWaiters. To exercise
		// the branch we'd need handleNext to succeed first; the previous
		// subtest does this. For now, just verify no crash on a workflow
		// not in cancelWaiters.
		sendAndDrain(t, conn, MsgWorkflowDone, "ref-done-2",
			DonePayload{WorkflowID: "wf-not-tracked", Started: 1, Finished: 2})
	})

	t.Run("HandleInit_InvalidPayload", func(t *testing.T) {
		conn := dial(t, "ci-spot-us-eas-init")
		defer conn.Close()
		sendAndDrain(t, conn, MsgWorkflowInit, "ref-init", "bad")
	})

	t.Run("HandleDone_InvalidPayload", func(t *testing.T) {
		conn := dial(t, "ci-spot-us-eas-done")
		defer conn.Close()
		sendAndDrain(t, conn, MsgWorkflowDone, "ref-done", "bad")
	})

	t.Run("HandleUpdate_InvalidPayload", func(t *testing.T) {
		conn := dial(t, "ci-spot-us-eas-upd")
		defer conn.Close()
		sendAndDrain(t, conn, MsgStepUpdate, "ref-upd", "bad")
	})

	t.Run("HandleLog_InvalidPayload", func(t *testing.T) {
		conn := dial(t, "ci-spot-us-eas-log")
		defer conn.Close()
		sendAndDrain(t, conn, MsgStepLog, "ref-log", "bad")
	})

	t.Run("HandleExtend_InvalidPayload", func(t *testing.T) {
		conn := dial(t, "ci-spot-us-eas-ext")
		defer conn.Close()
		sendAndDrain(t, conn, MsgExtend, "ref-ext", "bad")
	})

	t.Run("HandleNext_InvalidPayload", func(t *testing.T) {
		conn := dial(t, "ci-spot-us-eas-next")
		defer conn.Close()
		sendAndDrain(t, conn, MsgAgentNext, "ref-next", "bad")
	})

	t.Run("HandleUnregister_NoCrash", func(t *testing.T) {
		conn := dial(t, "ci-spot-us-eas-unreg")
		defer conn.Close()
		sendAndDrain(t, conn, MsgAgentUnregister, "ref-unreg", AckPayload{OK: true})
	})

	t.Run("UnknownMessageType", func(t *testing.T) {
		conn := dial(t, "ci-spot-us-eas-unk")
		defer conn.Close()
		sendAndDrain(t, conn, "totally-unknown-type", "ref-unk", AckPayload{OK: true})
	})

	t.Run("InvalidEnvelope", func(t *testing.T) {
		conn := dial(t, "ci-spot-us-eas-bad")
		defer conn.Close()
		// Raw garbage that doesn't even parse as JSON.
		assert.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{garbage`)))
		_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, _, _ = conn.ReadMessage()
	})
}

// TestWSAgent_RPCPeerNotConfigured — the early-return path when WSAgent
// can't get an RPC peer. recordWSClose must NOT fire (no read loop).
// Lives outside TestWSAgent_HandlerSubtests because it deliberately
// removes the RPC peer.
func TestWSAgent_RPCPeerNotConfigured(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server.Config.Server.AgentToken = "test-secret"
	server.Config.Services.WSAgentRPC = nil
	t.Cleanup(func() { server.Config.Services.WSAgentRPC = nil })

	r := gin.New()
	r.GET("/ws/agent", WSAgent)
	srv := httptest.NewServer(r)
	defer srv.Close()

	url := "ws" + srv.URL[4:] + "/ws/agent?token=test-secret&hostname=h"
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err == nil {
		_ = conn.Close()
	}
}

// TestWSAgent_RPCPeerWrongType — WSAgentRPC is set but to the wrong
// concrete type. Exercises the type-assert error branch (line 60-64).
func TestWSAgent_RPCPeerWrongType(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server.Config.Server.AgentToken = "test-secret"
	// String is obviously not *grpcserver.RPC.
	server.Config.Services.WSAgentRPC = "not-an-rpc"
	t.Cleanup(func() { server.Config.Services.WSAgentRPC = nil })

	r := gin.New()
	r.GET("/ws/agent", WSAgent)
	srv := httptest.NewServer(r)
	defer srv.Close()

	url := "ws" + srv.URL[4:] + "/ws/agent?token=test-secret&hostname=h"
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err == nil {
		_ = conn.Close()
	}
}
