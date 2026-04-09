package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog/log"
)

const (
	wsHeartbeatInterval = 10 * time.Second
	wsReconnectDelay    = 5 * time.Second
	wsWriteTimeout      = 5 * time.Second
)

// WSHeartbeatClient maintains a WebSocket connection to the server
// and sends heartbeats with running workflow IDs every 10s.
// The server extends queue leases on receipt, bypassing gRPC Extend.
type WSHeartbeatClient struct {
	serverURL string // gRPC server address (host:port)
	token     string // WOODPECKER_AGENT_SECRET
	agentID   int64
	hostname  string
	state     *State
	secure    bool
}

// NewWSHeartbeatClient creates a WebSocket heartbeat client.
func NewWSHeartbeatClient(serverAddr, token string, agentID int64, hostname string, state *State, secure bool) *WSHeartbeatClient {
	return &WSHeartbeatClient{
		serverURL: serverAddr,
		token:     token,
		agentID:   agentID,
		hostname:  hostname,
		state:     state,
		secure:    secure,
	}
}

// Run connects to the server and sends heartbeats until ctx is canceled.
// Reconnects on disconnection with exponential backoff.
func (c *WSHeartbeatClient) Run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}

		if err := c.connectAndHeartbeat(ctx); err != nil {
			log.Warn().Err(err).Msg("ws-heartbeat: disconnected, reconnecting...")
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(wsReconnectDelay):
		}
	}
}

func (c *WSHeartbeatClient) connectAndHeartbeat(ctx context.Context) error {
	wsURL := c.buildURL()
	log.Debug().Str("url", wsURL).Msg("ws-heartbeat: connecting")

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	log.Info().Msg("ws-heartbeat: connected")

	ticker := time.NewTicker(wsHeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			conn.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			return nil

		case <-ticker.C:
			if err := c.sendHeartbeat(conn); err != nil {
				return fmt.Errorf("send: %w", err)
			}
		}
	}
}

func (c *WSHeartbeatClient) sendHeartbeat(conn *websocket.Conn) error {
	workflowIDs := c.state.RunningIDs()

	msg := wsHeartbeatMsg{
		Hostname:    c.hostname,
		AgentID:     c.agentID,
		WorkflowIDs: workflowIDs,
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		return err
	}

	// Read ack (with timeout)
	conn.SetReadDeadline(time.Now().Add(wsWriteTimeout))
	_, ackData, err := conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("read ack: %w", err)
	}

	var ack wsHeartbeatAck
	if err := json.Unmarshal(ackData, &ack); err != nil {
		return fmt.Errorf("parse ack: %w", err)
	}

	if !ack.OK && ack.Error != "" {
		log.Warn().Str("error", ack.Error).Msg("ws-heartbeat: server rejected heartbeat")
	}

	if len(workflowIDs) > 0 {
		log.Debug().Int("extended", ack.Extended).Int("workflows", len(workflowIDs)).
			Msg("ws-heartbeat: leases extended")
	}

	return nil
}

func (c *WSHeartbeatClient) buildURL() string {
	// Convert gRPC server address to WebSocket URL.
	// In production: agent connects to d3ci42:443 via Caddy for both gRPC and HTTP.
	// Caddy routes /ws/heartbeat to the HTTP server (port 8000).
	// The WebSocket connection goes through the same TLS endpoint.
	host := c.serverURL

	// Strip any grpc:// or dns:/// prefix
	host = strings.TrimPrefix(host, "dns:///")
	host = strings.TrimPrefix(host, "grpc://")

	scheme := "ws"
	if c.secure {
		scheme = "wss"
	}

	params := url.Values{}
	params.Set("token", c.token)

	return fmt.Sprintf("%s://%s/ws/heartbeat?%s", scheme, host, params.Encode())
}

// Message types matching server/api/ws_heartbeat.go
type wsHeartbeatMsg struct {
	Hostname    string   `json:"hostname"`
	AgentID     int64    `json:"agent_id"`
	WorkflowIDs []string `json:"workflow_ids"`
}

type wsHeartbeatAck struct {
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
	Extended int    `json:"extended"`
}
