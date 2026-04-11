package rpc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cenkalti/backoff/v5"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog/log"

	backend "go.woodpecker-ci.org/woodpecker/v3/pipeline/backend/types"
	"go.woodpecker-ci.org/woodpecker/v3/rpc"
)

const (
	wsWriteTimeout   = 5 * time.Second
	wsReadTimeout    = 0 // no read timeout — server pushes async
	wsReconnectMin   = 5 * time.Second
	wsReconnectMax   = 60 * time.Second
	wsLogBatchSize   = 1 * 1024 * 1024 // 1 MiB
	wsLogFlushPeriod = time.Second
	wsHealthInterval = 10 * time.Second
)

// Envelope matches server/api/ws_protocol.go
type envelope struct {
	Type    string          `json:"type"`
	Ref     string          `json:"ref,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// WSClient implements rpc.Peer over WebSocket.
type WSClient struct {
	serverURL string
	token     string
	hostname  string
	secure    bool

	mu      sync.Mutex
	conn    *websocket.Conn
	agentID int64
	refSeq  atomic.Int64

	// Server → agent message routing
	inbox     chan envelope            // all incoming messages
	pending   map[string]chan envelope // ref → response channel
	pendingMu sync.Mutex
	cancels   map[string]chan struct{} // workflowID → cancel signal
	cancelsMu sync.Mutex

	// Log batching
	logs chan *rpc.LogEntry

	// Connection lifecycle
	connCtx            context.Context
	connCancel         context.CancelFunc
	lastConnectedAt    time.Time // when the last connection was established
	shortLivedFailures int       // consecutive connections that dropped immediately
}

// NewWSClient creates a WebSocket-based rpc.Peer.
func NewWSClient(ctx context.Context, serverAddr, token, hostname string, secure bool) rpc.Peer {
	c := &WSClient{
		serverURL: serverAddr,
		token:     token,
		hostname:  hostname,
		secure:    secure,
		inbox:     make(chan envelope, 64),
		pending:   make(map[string]chan envelope),
		cancels:   make(map[string]chan struct{}),
		logs:      make(chan *rpc.LogEntry, 10),
	}
	go c.processLogs(ctx)
	return c
}

// connect establishes the WebSocket connection.
func (c *WSClient) connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		c.conn.Close()
	}

	wsURL := c.buildURL()
	log.Debug().Str("url", wsURL).Msg("ws-client: connecting")

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return fmt.Errorf("ws-client: dial: %w", err)
	}

	c.conn = conn
	c.connCtx, c.connCancel = context.WithCancel(ctx)
	c.lastConnectedAt = time.Now()

	// Start read pump
	go c.readPump()

	log.Info().Msg("ws-client: connected")
	return nil
}

func (c *WSClient) ensureConnected(ctx context.Context) error {
	c.mu.Lock()
	connected := c.conn != nil
	lastConnected := c.lastConnectedAt
	c.mu.Unlock()

	if connected {
		return nil
	}

	// #3: If the last connection was very short-lived, the server isn't ready.
	// Apply linear backoff (5s, 10s, 15s... up to 60s) to avoid tight reconnect loops.
	if !lastConnected.IsZero() && time.Since(lastConnected) < wsReconnectMin {
		c.mu.Lock()
		c.shortLivedFailures++
		failures := c.shortLivedFailures
		c.mu.Unlock()

		delay := time.Duration(failures) * wsReconnectMin
		if delay > wsReconnectMax {
			delay = wsReconnectMax
		}
		log.Warn().Int("failures", failures).Dur("delay", delay).
			Msg("ws-client: short-lived connection, backing off")
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	} else if !lastConnected.IsZero() {
		// Connection lasted long enough — reset failure counter
		c.mu.Lock()
		c.shortLivedFailures = 0
		c.mu.Unlock()
	}

	b := backoff.NewExponentialBackOff()
	b.InitialInterval = wsReconnectMin
	b.MaxInterval = wsReconnectMax

	for {
		if err := c.connect(ctx); err != nil {
			log.Warn().Err(err).Msg("ws-client: connect failed, retrying")
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(b.NextBackOff()):
				continue
			}
		}
		return nil
	}
}

// readPump reads messages from the WebSocket and routes them.
func (c *WSClient) readPump() {
	defer func() {
		c.mu.Lock()
		if c.conn != nil {
			c.conn.Close()
			c.conn = nil
		}
		if c.connCancel != nil {
			c.connCancel()
		}
		c.mu.Unlock()

		// Unblock all pending request/response channels so callers can reconnect.
		// Without this, Next() hangs forever after server restart.
		c.pendingMu.Lock()
		for ref, ch := range c.pending {
			close(ch)
			delete(c.pending, ref)
		}
		c.pendingMu.Unlock()
	}()

	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err,
				websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				log.Warn().Err(err).Msg("ws-client: read error")
			}
			return
		}

		var env envelope
		if err := json.Unmarshal(message, &env); err != nil {
			log.Warn().Err(err).Msg("ws-client: invalid message")
			continue
		}

		// Route by ref (response to a request) or by type (server push)
		if env.Ref != "" {
			c.pendingMu.Lock()
			if ch, ok := c.pending[env.Ref]; ok {
				ch <- env
				delete(c.pending, env.Ref)
			}
			c.pendingMu.Unlock()
		}

		// Also handle server-push messages
		switch env.Type {
		case "task.cancel":
			var p struct {
				WorkflowID string `json:"workflow_id"`
			}
			if json.Unmarshal(env.Payload, &p) == nil {
				c.cancelsMu.Lock()
				if ch, ok := c.cancels[p.WorkflowID]; ok {
					close(ch)
					delete(c.cancels, p.WorkflowID)
				}
				c.cancelsMu.Unlock()
			}
		default:
			// Route to inbox for Next() and other blocking receivers
			select {
			case c.inbox <- env:
			default:
				log.Warn().Str("type", env.Type).Msg("ws-client: inbox full, dropping message")
			}
		}
	}
}

func (c *WSClient) nextRef() string {
	return fmt.Sprintf("r%d", c.refSeq.Add(1))
}

// send sends a message and optionally waits for an ack.
func (c *WSClient) send(msgType string, payload interface{}) (string, error) {
	ref := c.nextRef()
	raw, err := json.Marshal(payload)
	if err != nil {
		return ref, err
	}
	data, err := json.Marshal(envelope{Type: msgType, Ref: ref, Payload: raw})
	if err != nil {
		return ref, err
	}

	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return ref, fmt.Errorf("not connected")
	}

	conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	return ref, conn.WriteMessage(websocket.TextMessage, data)
}

// sendAndWait sends a message and waits for the ack.
func (c *WSClient) sendAndWait(ctx context.Context, msgType string, payload interface{}) error {
	ref, err := c.send(msgType, payload)
	if err != nil {
		return err
	}

	ch := make(chan envelope, 1)
	c.pendingMu.Lock()
	c.pending[ref] = ch
	c.pendingMu.Unlock()

	select {
	case env, ok := <-ch:
		if !ok {
			return fmt.Errorf("connection lost")
		}
		if env.Type == "ack" {
			var ack struct {
				OK    bool   `json:"ok"`
				Error string `json:"error"`
			}
			if json.Unmarshal(env.Payload, &ack) == nil && !ack.OK {
				return fmt.Errorf("server: %s", ack.Error)
			}
		}
		return nil
	case <-ctx.Done():
		c.pendingMu.Lock()
		delete(c.pending, ref)
		c.pendingMu.Unlock()
		return ctx.Err()
	case <-time.After(30 * time.Second):
		c.pendingMu.Lock()
		delete(c.pending, ref)
		c.pendingMu.Unlock()
		return fmt.Errorf("ack timeout")
	}
}

// ── rpc.Peer implementation ──────────────────────────────────────

func (c *WSClient) Version(ctx context.Context) (*rpc.Version, error) {
	// Server sends version on connect — read from inbox
	if err := c.ensureConnected(ctx); err != nil {
		return nil, err
	}

	select {
	case env := <-c.inbox:
		if env.Type == "version" {
			var v struct {
				ServerVersion string `json:"server_version"`
			}
			json.Unmarshal(env.Payload, &v)
			return &rpc.Version{ServerVersion: v.ServerVersion}, nil
		}
		return &rpc.Version{ServerVersion: "unknown"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(10 * time.Second):
		return &rpc.Version{ServerVersion: "unknown"}, nil
	}
}

func (c *WSClient) RegisterAgent(ctx context.Context, info rpc.AgentInfo) (int64, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return 0, err
	}

	ref, err := c.send("agent.register", info)
	if err != nil {
		return 0, err
	}

	ch := make(chan envelope, 1)
	c.pendingMu.Lock()
	c.pending[ref] = ch
	c.pendingMu.Unlock()

	select {
	case env, ok := <-ch:
		if !ok {
			return 0, fmt.Errorf("connection lost during registration")
		}
		if env.Type == "registered" {
			var p struct {
				AgentID int64 `json:"agent_id"`
			}
			json.Unmarshal(env.Payload, &p)
			c.agentID = p.AgentID
			return p.AgentID, nil
		}
		// Check for ack error
		var ack struct {
			Error string `json:"error"`
		}
		json.Unmarshal(env.Payload, &ack)
		if ack.Error != "" {
			return 0, fmt.Errorf("register: %s", ack.Error)
		}
		return 0, fmt.Errorf("unexpected response: %s", env.Type)
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-time.After(10 * time.Second):
		return 0, fmt.Errorf("register timeout")
	}
}

func (c *WSClient) UnregisterAgent(ctx context.Context) error {
	return c.sendAndWait(ctx, "agent.unregister", nil)
}

func (c *WSClient) Next(ctx context.Context, f rpc.Filter) (*rpc.Workflow, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return nil, nil
	}

	ref, err := c.send("agent.next", struct {
		FilterLabels map[string]string `json:"filter_labels"`
	}{FilterLabels: f.Labels})
	if err != nil {
		// Connection error — reconnect and retry (don't return error to caller)
		c.mu.Lock()
		if c.conn != nil {
			c.conn.Close()
			c.conn = nil
		}
		c.mu.Unlock()
		return nil, nil
	}

	ch := make(chan envelope, 1)
	c.pendingMu.Lock()
	c.pending[ref] = ch
	c.pendingMu.Unlock()

	select {
	case env, ok := <-ch:
		if !ok {
			// Channel closed — connection dropped, readPump cleaned up.
			// Return nil to trigger reconnect on next runner iteration.
			return nil, nil
		}
		if env.Type == "task.assign" && env.Payload != nil {
			var p struct {
				ID      string          `json:"id"`
				Timeout int64           `json:"timeout"`
				Config  json.RawMessage `json:"config"`
			}
			if err := json.Unmarshal(env.Payload, &p); err != nil || p.ID == "" {
				return nil, nil // no work
			}
			var config *backend.Config
			if len(p.Config) > 0 {
				config = new(backend.Config)
				json.Unmarshal(p.Config, config)
			}
			return &rpc.Workflow{ID: p.ID, Timeout: p.Timeout, Config: config}, nil
		}
		return nil, nil
	case <-ctx.Done():
		c.pendingMu.Lock()
		delete(c.pending, ref)
		c.pendingMu.Unlock()
		return nil, nil
	}
}

func (c *WSClient) Wait(ctx context.Context, workflowID string) (bool, error) {
	// Subscribe to cancel signal for this workflow
	ch := make(chan struct{})
	c.cancelsMu.Lock()
	c.cancels[workflowID] = ch
	c.cancelsMu.Unlock()

	defer func() {
		c.cancelsMu.Lock()
		delete(c.cancels, workflowID)
		c.cancelsMu.Unlock()
	}()

	select {
	case <-ch:
		// Server sent task.cancel for this workflow
		return true, nil
	case <-ctx.Done():
		// Context canceled (workflow completed or agent shutdown)
		// CRITICAL: Do NOT return error — this is the fix for #3496/#3497.
		// gRPC client returns (false, err) on disconnect → agent self-kills workflow.
		// WS client returns (false, nil) → workflow continues.
		return false, nil
	}
}

func (c *WSClient) Init(ctx context.Context, workflowID string, state rpc.WorkflowState) error {
	return c.sendWithRetry(ctx, "workflow.init", struct {
		WorkflowID string `json:"workflow_id"`
		Started    int64  `json:"started"`
	}{WorkflowID: workflowID, Started: state.Started})
}

func (c *WSClient) Done(ctx context.Context, workflowID string, state rpc.WorkflowState) error {
	return c.sendWithRetry(ctx, "workflow.done", struct {
		WorkflowID string `json:"workflow_id"`
		Started    int64  `json:"started"`
		Finished   int64  `json:"finished"`
		Error      string `json:"error,omitempty"`
		Canceled   bool   `json:"canceled,omitempty"`
	}{
		WorkflowID: workflowID,
		Started:    state.Started,
		Finished:   state.Finished,
		Error:      state.Error,
		Canceled:   state.Canceled,
	})
}

func (c *WSClient) Extend(ctx context.Context, workflowID string) error {
	_, err := c.send("extend", struct {
		WorkflowIDs []string `json:"workflow_ids"`
	}{WorkflowIDs: []string{workflowID}})
	return err
}

func (c *WSClient) Update(ctx context.Context, workflowID string, state rpc.StepState) error {
	return c.sendWithRetry(ctx, "step.update", struct {
		WorkflowID string `json:"workflow_id"`
		StepUUID   string `json:"step_uuid"`
		Started    int64  `json:"started,omitempty"`
		Finished   int64  `json:"finished,omitempty"`
		Exited     bool   `json:"exited,omitempty"`
		ExitCode   int    `json:"exit_code,omitempty"`
		Error      string `json:"error,omitempty"`
		Canceled   bool   `json:"canceled,omitempty"`
	}{
		WorkflowID: workflowID,
		StepUUID:   state.StepUUID,
		Started:    state.Started,
		Finished:   state.Finished,
		Exited:     state.Exited,
		ExitCode:   state.ExitCode,
		Error:      state.Error,
		Canceled:   state.Canceled,
	})
}

func (c *WSClient) EnqueueLog(logEntry *rpc.LogEntry) {
	c.logs <- logEntry
}

func (c *WSClient) ReportHealth(ctx context.Context) error {
	// Collect running workflow IDs for piggybacked extend
	c.cancelsMu.Lock()
	wfIDs := make([]string, 0, len(c.cancels))
	for id := range c.cancels {
		wfIDs = append(wfIDs, id)
	}
	c.cancelsMu.Unlock()

	_, err := c.send("health", struct {
		WorkflowIDs []string `json:"workflow_ids,omitempty"`
	}{WorkflowIDs: wfIDs})
	return err
}

// ── Internal helpers ─────────────────────────────────────────────

func (c *WSClient) sendWithRetry(ctx context.Context, msgType string, payload interface{}) error {
	b := backoff.NewExponentialBackOff()
	b.InitialInterval = 10 * time.Millisecond
	b.MaxInterval = 10 * time.Second

	for {
		if err := c.ensureConnected(ctx); err != nil {
			return err
		}

		err := c.sendAndWait(ctx, msgType, payload)
		if err == nil {
			return nil
		}

		if ctx.Err() != nil {
			return ctx.Err()
		}

		log.Warn().Err(err).Str("type", msgType).Msg("ws-client: retrying")

		// Connection might be dead — clear it for reconnect
		c.mu.Lock()
		if c.conn != nil {
			c.conn.Close()
			c.conn = nil
		}
		c.mu.Unlock()

		select {
		case <-time.After(b.NextBackOff()):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (c *WSClient) processLogs(ctx context.Context) {
	var entries []*rpc.LogEntry
	var stepUUID string
	var bytes int

	send := func() {
		if len(entries) == 0 {
			return
		}

		wsEntries := make([]struct {
			Time int64  `json:"time,omitempty"`
			Type int    `json:"type,omitempty"`
			Line int    `json:"line,omitempty"`
			Data []byte `json:"data,omitempty"`
		}, len(entries))
		for i, e := range entries {
			wsEntries[i].Time = e.Time
			wsEntries[i].Type = e.Type
			wsEntries[i].Line = e.Line
			wsEntries[i].Data = e.Data
		}

		payload := struct {
			StepUUID string      `json:"step_uuid"`
			Entries  interface{} `json:"entries"`
		}{StepUUID: stepUUID, Entries: wsEntries}

		if _, err := c.send("step.log", payload); err != nil {
			log.Error().Err(err).Msg("ws-client: log send failed")
		}
		entries = entries[:0]
		bytes = 0
	}

	for {
		select {
		case entry, ok := <-c.logs:
			if !ok {
				send()
				return
			}
			if entry.StepUUID != stepUUID && len(entries) > 0 {
				send()
			}
			stepUUID = entry.StepUUID
			entries = append(entries, entry)
			bytes += len(entry.Data) + 64 // rough estimate
			if bytes >= wsLogBatchSize {
				send()
			}
		case <-time.After(wsLogFlushPeriod):
			send()
		}
	}
}

func (c *WSClient) buildURL() string {
	host := c.serverURL
	host = strings.TrimPrefix(host, "dns:///")
	host = strings.TrimPrefix(host, "grpc://")

	scheme := "ws"
	if c.secure {
		scheme = "wss"
	}

	params := url.Values{}
	params.Set("token", c.token)
	params.Set("hostname", c.hostname)

	return fmt.Sprintf("%s://%s/ws/agent?%s", scheme, host, params.Encode())
}
