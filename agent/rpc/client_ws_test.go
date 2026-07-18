package rpc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"

	"go.woodpecker-ci.org/woodpecker/v3/rpc"
)

// inboxFullCounter is a zerolog hook that counts "inbox full" warnings race-free,
// so tests can assert the #295 fan-out fix emits none. It is installed ONCE on the
// global logger in TestMain (never swapped per-test), so concurrent readPump
// goroutines from other tests read an unchanging log.Logger — no data race.
type inboxFullCounter struct{ n atomic.Int64 }

func (h *inboxFullCounter) Run(_ *zerolog.Event, _ zerolog.Level, msg string) {
	if msg == "ws-client: inbox full, dropping message" {
		h.n.Add(1)
	}
}

var inboxFullWarns = &inboxFullCounter{}

func TestMain(m *testing.M) {
	log.Logger = log.Logger.Hook(inboxFullWarns)
	os.Exit(m.Run())
}

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

// holdUntilCleanup returns a channel a mock WS handler blocks on (`<-hold`)
// after writing its frame(s), so the connection stays fully open until the test
// finishes. It replaces a fixed `time.Sleep(100ms)` before the handler returns
// (and `defer conn.Close()` severs the socket): under CI load the client's
// readPump could be scheduled >100ms late, so the close raced — and beat — the
// just-written frame, and the client surfaced "connection lost" instead of the
// semantic reply. Blocking until cleanup removes the race entirely; the channel
// is closed in t.Cleanup, letting the handler return and close the conn. Safe
// against srv.Close() hanging because an upgraded WS conn is hijacked and no
// longer tracked by httptest's connection accounting (#325).
func holdUntilCleanup(t *testing.T) chan struct{} {
	t.Helper()
	hold := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(hold) }) })
	return hold
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
	hold := holdUntilCleanup(t)
	srv := mockWSServer(t, func(conn *websocket.Conn) {
		// Send version message
		env, _ := json.Marshal(envelope{
			Type:    "version",
			Payload: json.RawMessage(`{"server_version":"v3.13.0-pts.12"}`),
		})
		conn.WriteMessage(websocket.TextMessage, env)
		<-hold // keep the connection open until cleanup (see holdUntilCleanup)
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

// TestWSClient_RefBearingEnvelopesDoNotFillInbox is the #295 regression guard.
// Ref-bearing responses are delivered to pending[ref] and must NOT also be fanned
// into the 64-buffered inbox. Before the fix, every envelope past ~64 emitted
// "ws-client: inbox full, dropping message" forever, falsely implicating agent IO.
func TestWSClient_RefBearingEnvelopesDoNotFillInbox(t *testing.T) {
	const n = 100

	warnsBefore := inboxFullWarns.n.Load()
	delivered := make(chan string, n)

	srv := mockWSServer(t, func(conn *websocket.Conn) {
		// Push n ref-bearing responses — far more than the inbox's cap of 64.
		for i := 1; i <= n; i++ {
			env, _ := json.Marshal(envelope{
				Type:    "task.assign",
				Ref:     fmt.Sprintf("r%d", i),
				Payload: json.RawMessage(`{}`),
			})
			conn.WriteMessage(websocket.TextMessage, env)
		}
		time.Sleep(500 * time.Millisecond) // keep conn alive while readPump drains
	})
	defer srv.Close()

	wsURL := srv.URL[7:]
	c := NewWSClient(context.Background(), wsURL, "test-secret", "agent-1", false).(*WSClient)

	// Register a pending channel for every ref BEFORE connecting, so readPump
	// finds each one and routes deterministically.
	for i := 1; i <= n; i++ {
		ch := make(chan envelope, 1)
		ref := fmt.Sprintf("r%d", i)
		c.pendingMu.Lock()
		c.pending[ref] = ch
		c.pendingMu.Unlock()
		go func() { delivered <- (<-ch).Ref }()
	}

	c.connect(context.Background())

	seen := make(map[string]bool, n)
	deadline := time.After(5 * time.Second)
	for len(seen) < n {
		select {
		case ref := <-delivered:
			seen[ref] = true
		case <-deadline:
			t.Fatalf("only %d/%d ref-bearing envelopes delivered via pending[ref]", len(seen), n)
		}
	}

	assert.Equal(t, n, len(seen), "all ref-bearing envelopes delivered via pending[ref]")
	assert.Equal(t, int64(0), inboxFullWarns.n.Load()-warnsBefore, "zero 'inbox full' warnings for ref-bearing envelopes")
	assert.Equal(t, 0, len(c.inbox), "ref-bearing envelopes must not leak into the inbox")
}

// msgRecorder thread-safely records the message types a mock server received,
// so a test goroutine can assert against them while the server goroutine appends.
type msgRecorder struct {
	mu    sync.Mutex
	types []string
}

func (r *msgRecorder) record(env envelope) {
	r.mu.Lock()
	r.types = append(r.types, env.Type)
	r.mu.Unlock()
}

func (r *msgRecorder) has(t string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ty := range r.types {
		if ty == t {
			return true
		}
	}
	return false
}

// autoAckWSServer connects a client to a mock server that replies to every
// ref-bearing request with an "ack" {ok:true} (or a "task.assign" for
// agent.next). It drives the sendAndWait/send happy paths for the RPC methods.
// onMsg, if non-nil, observes each decoded request the server received.
func autoAckWSServer(t *testing.T, onMsg func(envelope)) *WSClient {
	t.Helper()
	srv := mockWSServer(t, func(conn *websocket.Conn) {
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var env envelope
			if json.Unmarshal(msg, &env) != nil {
				continue
			}
			if onMsg != nil {
				onMsg(env)
			}
			if env.Ref == "" {
				continue // fire-and-forget (extend, health) — nothing to ack
			}
			replyType := "ack"
			payload := json.RawMessage(`{"ok":true}`)
			if env.Type == "agent.next" {
				replyType = "task.assign"
				payload = json.RawMessage(`{"id":"wf-7","timeout":60,"config":{}}`)
			}
			resp, _ := json.Marshal(envelope{Type: replyType, Ref: env.Ref, Payload: payload})
			conn.WriteMessage(websocket.TextMessage, resp)
		}
	})
	t.Cleanup(srv.Close)

	c := NewWSClient(context.Background(), srv.URL[7:], "test-secret", "agent-1", false).(*WSClient)
	if err := c.connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	return c
}

func TestWSClient_InitDoneUpdate_SendAndWaitAck(t *testing.T) {
	rec := &msgRecorder{}
	c := autoAckWSServer(t, rec.record)

	// autoAckWSServer acks deterministically from a read loop that holds the
	// connection open, so these four round-trips always complete — the only way
	// to miss the deadline is CI-VM scheduling starvation, not a real stall.
	// Give generous headroom (the op is sub-millisecond; a genuine hang still
	// fails at this bound) rather than flaking a correct client under load (#325).
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	assert.NoError(t, c.Init(ctx, "wf-1", rpc.WorkflowState{Started: 1}))
	assert.NoError(t, c.Done(ctx, "wf-1", rpc.WorkflowState{Started: 1, Finished: 2, Canceled: true, Error: "boom"}))
	assert.NoError(t, c.Update(ctx, "wf-1", rpc.StepState{StepUUID: "s-1", Exited: true, ExitCode: 0}))
	assert.NoError(t, c.UnregisterAgent(ctx))

	assert.True(t, rec.has("workflow.init"), "server received workflow.init")
	assert.True(t, rec.has("workflow.done"), "server received workflow.done")
	assert.True(t, rec.has("step.update"), "server received step.update")
	assert.True(t, rec.has("agent.unregister"), "server received agent.unregister")
}

func TestWSClient_Next_ReturnsAssignedWorkflow(t *testing.T) {
	c := autoAckWSServer(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wf, err := c.Next(ctx, rpc.Filter{Labels: map[string]string{"tier": "spot"}})
	assert.NoError(t, err)
	if assert.NotNil(t, wf) {
		assert.Equal(t, "wf-7", wf.ID)
		assert.Equal(t, int64(60), wf.Timeout)
	}
}

func TestWSClient_ExtendAndReportHealth_FireAndForget(t *testing.T) {
	rec := &msgRecorder{}
	c := autoAckWSServer(t, rec.record)

	// Register a running workflow so ReportHealth piggybacks its ID.
	c.cancelsMu.Lock()
	c.cancels["wf-9"] = make(chan struct{})
	c.cancelsMu.Unlock()

	ctx := context.Background()
	assert.NoError(t, c.Extend(ctx, "wf-9"))
	assert.NoError(t, c.ReportHealth(ctx))

	assert.Eventually(t, func() bool { return rec.has("health") && rec.has("extend") },
		2*time.Second, 10*time.Millisecond, "server should receive extend + health messages")
}

func TestWSClient_EnqueueLog_BatchesToServer(t *testing.T) {
	var stepLogs atomic.Int64
	c := autoAckWSServer(t, func(env envelope) {
		if env.Type == "step.log" {
			stepLogs.Add(1)
		}
	})

	// Two different steps → forces a flush on stepUUID change, then a timer flush.
	c.EnqueueLog(&rpc.LogEntry{StepUUID: "s-1", Data: []byte("line a")})
	c.EnqueueLog(&rpc.LogEntry{StepUUID: "s-2", Data: []byte("line b")})

	assert.Eventually(t, func() bool { return stepLogs.Load() >= 1 },
		3*time.Second, 20*time.Millisecond, "processLogs should flush step.log batches to the server")
}

func TestWSClient_SendAndWait_AckErrorSurfaces(t *testing.T) {
	hold := holdUntilCleanup(t)
	srv := mockWSServer(t, func(conn *websocket.Conn) {
		_, msg, _ := conn.ReadMessage()
		var env envelope
		json.Unmarshal(msg, &env)
		resp, _ := json.Marshal(envelope{Type: "ack", Ref: env.Ref, Payload: json.RawMessage(`{"ok":false,"error":"nope"}`)})
		conn.WriteMessage(websocket.TextMessage, resp)
		<-hold // keep the connection open until cleanup (see holdUntilCleanup)
	})
	defer srv.Close()

	c := NewWSClient(context.Background(), srv.URL[7:], "test-secret", "agent-1", false).(*WSClient)
	c.connect(context.Background())

	err := c.sendAndWait(context.Background(), "workflow.init", struct{}{})
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "nope")
	}
}

func TestWSClient_ReadPump_RoutesPushesAndClosesPendingOnDisconnect(t *testing.T) {
	// Server pushes a task.cancel (routed to cancels) then drops the connection;
	// readPump's defer must close any registered pending channel so callers unblock.
	srv := mockWSServer(t, func(conn *websocket.Conn) {
		env, _ := json.Marshal(envelope{
			Type:    "task.cancel",
			Payload: json.RawMessage(`{"workflow_id":"wf-c"}`),
		})
		conn.WriteMessage(websocket.TextMessage, env)
		time.Sleep(50 * time.Millisecond) // let readPump process before close
	})
	defer srv.Close()

	c := NewWSClient(context.Background(), srv.URL[7:], "test-secret", "agent-1", false).(*WSClient)

	cancelCh := make(chan struct{})
	c.cancelsMu.Lock()
	c.cancels["wf-c"] = cancelCh
	c.cancelsMu.Unlock()

	pendingCh := make(chan envelope, 1)
	c.pendingMu.Lock()
	c.pending["r-orphan"] = pendingCh
	c.pendingMu.Unlock()

	c.connect(context.Background())

	// task.cancel signal arrives.
	select {
	case <-cancelCh:
	case <-time.After(2 * time.Second):
		t.Fatal("task.cancel was not routed to the cancels channel")
	}

	// On disconnect, the orphan pending channel is closed (not left to hang).
	select {
	case _, ok := <-pendingCh:
		assert.False(t, ok, "pending channel should be closed on disconnect")
	case <-time.After(2 * time.Second):
		t.Fatal("pending channel was not closed on disconnect")
	}
}

func TestWSClient_ReadPump_InvalidJSONIsSkipped(t *testing.T) {
	delivered := make(chan struct{}, 1)
	hold := holdUntilCleanup(t)
	srv := mockWSServer(t, func(conn *websocket.Conn) {
		conn.WriteMessage(websocket.TextMessage, []byte("{not json"))
		good, _ := json.Marshal(envelope{Type: "version", Payload: json.RawMessage(`{"server_version":"v9"}`)})
		conn.WriteMessage(websocket.TextMessage, good)
		<-hold // keep the connection open until cleanup (see holdUntilCleanup)
	})
	defer srv.Close()

	c := NewWSClient(context.Background(), srv.URL[7:], "test-secret", "agent-1", false).(*WSClient)
	c.connect(context.Background())

	// The valid version push after the garbage must still arrive in the inbox.
	go func() {
		<-c.inbox
		delivered <- struct{}{}
	}()
	select {
	case <-delivered:
	case <-time.After(2 * time.Second):
		t.Fatal("valid envelope after invalid JSON was not processed")
	}
}

func TestWSClient_Send_NotConnectedAndMarshalError(t *testing.T) {
	c := &WSClient{} // no connection
	_, err := c.send("x", struct{}{})
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "not connected")
	}

	// json.Marshal fails on a channel value → send returns the marshal error.
	_, err = c.send("x", make(chan int))
	assert.Error(t, err, "unmarshalable payload should surface a marshal error")
}

func TestWSClient_Connect_DialError(t *testing.T) {
	// Dial failure returns an error (port 1 refuses).
	c := &WSClient{serverURL: "127.0.0.1:1", token: "t", hostname: "h"}
	assert.Error(t, c.connect(context.Background()), "dial to a dead port should error")
}

func TestWSClient_SendAndWait_ContextCancelClearsPending(t *testing.T) {
	// Server reads but never acks → sendAndWait blocks until ctx cancels.
	srv := mockWSServer(t, func(conn *websocket.Conn) {
		conn.ReadMessage()
		time.Sleep(500 * time.Millisecond)
	})
	defer srv.Close()
	c := NewWSClient(context.Background(), srv.URL[7:], "test-secret", "agent-1", false).(*WSClient)
	c.connect(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	err := c.sendAndWait(ctx, "workflow.init", struct{}{})
	assert.ErrorIs(t, err, context.DeadlineExceeded)

	c.pendingMu.Lock()
	n := len(c.pending)
	c.pendingMu.Unlock()
	assert.Equal(t, 0, n, "ctx cancel must delete the pending ref")
}

func TestWSClient_Version_NonVersionEnvelopeReturnsUnknown(t *testing.T) {
	hold := holdUntilCleanup(t)
	srv := mockWSServer(t, func(conn *websocket.Conn) {
		env, _ := json.Marshal(envelope{Type: "ping"}) // not "version"
		conn.WriteMessage(websocket.TextMessage, env)
		<-hold // keep the connection open until cleanup (see holdUntilCleanup)
	})
	defer srv.Close()
	c := NewWSClient(context.Background(), srv.URL[7:], "test-secret", "agent-1", false).(*WSClient)

	v, err := c.Version(context.Background())
	assert.NoError(t, err)
	assert.Equal(t, "unknown", v.ServerVersion)
}

func TestWSClient_RegisterAgent_ServerErrorSurfaces(t *testing.T) {
	hold := holdUntilCleanup(t)
	srv := mockWSServer(t, func(conn *websocket.Conn) {
		_, msg, _ := conn.ReadMessage()
		var env envelope
		json.Unmarshal(msg, &env)
		// Reply with an ack carrying an error (not a "registered" envelope).
		resp, _ := json.Marshal(envelope{Type: "ack", Ref: env.Ref, Payload: json.RawMessage(`{"error":"denied"}`)})
		conn.WriteMessage(websocket.TextMessage, resp)
		<-hold // keep the connection open until cleanup (see holdUntilCleanup)
	})
	defer srv.Close()
	c := NewWSClient(context.Background(), srv.URL[7:], "test-secret", "agent-1", false).(*WSClient)
	c.connect(context.Background())

	_, err := c.RegisterAgent(context.Background(), rpc.AgentInfo{Capacity: 1})
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "denied")
	}
}

func TestWSClient_Next_NoWorkOnEmptyAssign(t *testing.T) {
	hold := holdUntilCleanup(t)
	srv := mockWSServer(t, func(conn *websocket.Conn) {
		_, msg, _ := conn.ReadMessage()
		var env envelope
		json.Unmarshal(msg, &env)
		// task.assign with an empty id → no work.
		resp, _ := json.Marshal(envelope{Type: "task.assign", Ref: env.Ref, Payload: json.RawMessage(`{"id":""}`)})
		conn.WriteMessage(websocket.TextMessage, resp)
		<-hold // keep the connection open until cleanup (see holdUntilCleanup)
	})
	defer srv.Close()
	c := NewWSClient(context.Background(), srv.URL[7:], "test-secret", "agent-1", false).(*WSClient)
	c.connect(context.Background())

	wf, err := c.Next(context.Background(), rpc.Filter{})
	assert.NoError(t, err)
	assert.Nil(t, wf, "empty assign id means no work")
}

func TestWSClient_Next_ContextCancelClearsPending(t *testing.T) {
	srv := mockWSServer(t, func(conn *websocket.Conn) {
		conn.ReadMessage()
		time.Sleep(500 * time.Millisecond) // never replies
	})
	defer srv.Close()
	c := NewWSClient(context.Background(), srv.URL[7:], "test-secret", "agent-1", false).(*WSClient)
	c.connect(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	wf, err := c.Next(ctx, rpc.Filter{})
	assert.NoError(t, err, "Next never returns an error to the caller")
	assert.Nil(t, wf)
}

func TestWSClient_ReadPump_UnsolicitedPushOverflowWarns(t *testing.T) {
	// Genuinely unsolicited pushes (no ref) DO still legitimately warn when the
	// inbox overflows — the #295 fix only stops ref-bearing envelopes from
	// filling it. This guards that the warn path remains for real overflow.
	const pushes = 70 // > inbox cap of 64, and nothing drains it here

	warnsBefore := inboxFullWarns.n.Load()
	srv := mockWSServer(t, func(conn *websocket.Conn) {
		for i := 0; i < pushes; i++ {
			env, _ := json.Marshal(envelope{Type: "noop"}) // no ref, not task.cancel
			conn.WriteMessage(websocket.TextMessage, env)
		}
		time.Sleep(300 * time.Millisecond)
	})
	defer srv.Close()

	c := NewWSClient(context.Background(), srv.URL[7:], "test-secret", "agent-1", false).(*WSClient)
	c.connect(context.Background())

	assert.Eventually(t, func() bool { return inboxFullWarns.n.Load()-warnsBefore > 0 },
		2*time.Second, 20*time.Millisecond,
		"unsolicited pushes overflowing the inbox should still warn")
}

// readThenCloseWSServer reads one message then drops the connection without
// replying — readPump closes the waiting pending channel, exercising the
// "connection lost" branches of sendAndWait / Next / RegisterAgent fast.
func readThenCloseWSServer(t *testing.T) *WSClient {
	t.Helper()
	srv := mockWSServer(t, func(conn *websocket.Conn) {
		conn.ReadMessage() // then handler returns → conn closes
	})
	t.Cleanup(srv.Close)
	c := NewWSClient(context.Background(), srv.URL[7:], "test-secret", "agent-1", false).(*WSClient)
	if err := c.connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	return c
}

// readThenCloseWSServerKeepAlive connects a client to a server that reads but
// holds the connection open without replying, so a pre-canceled ctx wins the
// response select deterministically (not the disconnect/"connection lost" path).
func readThenCloseWSServerKeepAlive(t *testing.T) *WSClient {
	t.Helper()
	srv := mockWSServer(t, func(conn *websocket.Conn) {
		conn.ReadMessage()
		time.Sleep(500 * time.Millisecond) // hold open
	})
	t.Cleanup(srv.Close)
	c := NewWSClient(context.Background(), srv.URL[7:], "test-secret", "agent-1", false).(*WSClient)
	if err := c.connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	return c
}

func TestWSClient_SendAndWait_ConnectionLostOnDisconnect(t *testing.T) {
	c := readThenCloseWSServer(t)
	err := c.sendAndWait(context.Background(), "workflow.init", struct{}{})
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "connection lost")
	}
}

func TestWSClient_Next_ConnectionLostReturnsNil(t *testing.T) {
	c := readThenCloseWSServer(t)
	wf, err := c.Next(context.Background(), rpc.Filter{})
	assert.NoError(t, err, "Next returns nil error so the runner reconnects")
	assert.Nil(t, wf)
}

func TestWSClient_RegisterAgent_ConnectionLost(t *testing.T) {
	c := readThenCloseWSServer(t)
	_, err := c.RegisterAgent(context.Background(), rpc.AgentInfo{Capacity: 1})
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "connection lost")
	}
}

func TestWSClient_Version_ContextCancel(t *testing.T) {
	// Server connects but never sends a version → ctx cancel wins the select.
	c := readThenCloseWSServerKeepAlive(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Version(ctx)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestWSClient_RegisterAgent_ContextCancel(t *testing.T) {
	c := readThenCloseWSServerKeepAlive(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.RegisterAgent(ctx, rpc.AgentInfo{Capacity: 1})
	assert.ErrorIs(t, err, context.Canceled)
}

func TestWSClient_RegisterAgent_UnexpectedResponseType(t *testing.T) {
	hold := holdUntilCleanup(t)
	srv := mockWSServer(t, func(conn *websocket.Conn) {
		_, msg, _ := conn.ReadMessage()
		var env envelope
		json.Unmarshal(msg, &env)
		// Neither "registered" nor an ack carrying an error → "unexpected response".
		resp, _ := json.Marshal(envelope{Type: "weird", Ref: env.Ref, Payload: json.RawMessage(`{}`)})
		conn.WriteMessage(websocket.TextMessage, resp)
		<-hold // keep the connection open until cleanup (see holdUntilCleanup)
	})
	defer srv.Close()
	c := NewWSClient(context.Background(), srv.URL[7:], "test-secret", "agent-1", false).(*WSClient)
	c.connect(context.Background())

	_, err := c.RegisterAgent(context.Background(), rpc.AgentInfo{Capacity: 1})
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "unexpected response")
	}
}

func TestWSClient_EnsureConnected_RetriesThenContextCancels(t *testing.T) {
	oldMin, oldMax := wsReconnectMin, wsReconnectMax
	wsReconnectMin, wsReconnectMax = 5*time.Millisecond, 20*time.Millisecond
	defer func() { wsReconnectMin, wsReconnectMax = oldMin, oldMax }()

	// Dead address → connect() fails forever; the backoff loop retries until ctx
	// cancels, exercising the connect-failed + ctx.Done branch of ensureConnected.
	c := NewWSClient(context.Background(), "127.0.0.1:1", "test-secret", "agent-1", false).(*WSClient)
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	err := c.ensureConnected(ctx)
	assert.ErrorIs(t, err, context.DeadlineExceeded, "ensureConnected gives up when ctx expires mid-retry")
}

func TestWSClient_SendWithRetry_ReconnectsThenSucceeds(t *testing.T) {
	// Shrink the reconnect backoff so the retry loop runs in milliseconds.
	oldMin, oldMax := wsReconnectMin, wsReconnectMax
	wsReconnectMin, wsReconnectMax = 5*time.Millisecond, 20*time.Millisecond
	defer func() { wsReconnectMin, wsReconnectMax = oldMin, oldMax }()

	var conns atomic.Int64
	hold := holdUntilCleanup(t)
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
		n := conns.Add(1)
		_, msg, _ := conn.ReadMessage()
		if n == 1 {
			return // first attempt: drop the conn without acking → "connection lost"
		}
		var env envelope
		json.Unmarshal(msg, &env)
		resp, _ := json.Marshal(envelope{Type: "ack", Ref: env.Ref, Payload: json.RawMessage(`{"ok":true}`)})
		conn.WriteMessage(websocket.TextMessage, resp)
		<-hold // keep the connection open until cleanup (see holdUntilCleanup)
	}))
	defer srv.Close()

	c := NewWSClient(context.Background(), srv.URL[7:], "test-secret", "agent-1", false).(*WSClient)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Init goes through sendWithRetry: the first attempt's response is lost on the
	// dropped conn; the retry loop reconnects (ms backoff) and the second acks.
	assert.NoError(t, c.Init(ctx, "wf-1", rpc.WorkflowState{Started: 1}))
	assert.GreaterOrEqual(t, conns.Load(), int64(2), "sendWithRetry should have reconnected at least once")
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
