package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog/log"
	grpcMetadata "google.golang.org/grpc/metadata"

	"go.woodpecker-ci.org/woodpecker/v3/rpc"
	"go.woodpecker-ci.org/woodpecker/v3/server"
	grpcserver "go.woodpecker-ci.org/woodpecker/v3/server/rpc"
	"go.woodpecker-ci.org/woodpecker/v3/server/store"
	"go.woodpecker-ci.org/woodpecker/v3/version"
)

var agentUpgrader = websocket.Upgrader{
	CheckOrigin: func(_ *http.Request) bool { return true },
}

// WSAgent handles the full agent↔server WebSocket lifecycle.
// Replaces gRPC for agent communication (#474).
func WSAgent(c *gin.Context) {
	token := c.Query("token")
	if token == "" || token != server.Config.Server.AgentToken {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
		return
	}

	conn, err := agentUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Error().Err(err).Msg("ws-agent: upgrade failed")
		return
	}
	defer conn.Close()

	hostname := c.Query("hostname")
	log.Info().Str("hostname", hostname).Msg("ws-agent: connected")

	// Get the RPC peer and store from server config
	rpcPeerAny := server.Config.Services.WSAgentRPC
	if rpcPeerAny == nil {
		log.Error().Msg("ws-agent: RPC peer not configured")
		return
	}
	rpcPeer, ok := rpcPeerAny.(*grpcserver.RPC)
	if !ok {
		log.Error().Msg("ws-agent: RPC peer has wrong type")
		return
	}
	_store := store.FromContext(c)

	// Send version on connect
	sendEnvelope(conn, MsgVersion, "", VersionPayload{
		ServerVersion: version.String(),
	})

	// Agent state
	state := &wsAgentState{
		conn:          conn,
		rpc:           rpcPeer,
		store:         _store,
		cancelWaiters: make(map[string]context.CancelFunc),
	}

	// Read loop
	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err,
				websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				log.Warn().Err(err).Str("hostname", hostname).Msg("ws-agent: unexpected close")
			} else {
				log.Debug().Str("hostname", hostname).Msg("ws-agent: disconnected")
			}
			state.cleanup()
			return
		}

		var env Envelope
		if err := json.Unmarshal(message, &env); err != nil {
			sendAck(conn, env.Ref, "invalid envelope")
			continue
		}

		state.handleMessage(c.Request.Context(), env, hostname)
	}
}

// wsAgentState tracks per-connection state for one agent.
type wsAgentState struct {
	mu            sync.Mutex
	conn          *websocket.Conn
	rpc           *grpcserver.RPC
	store         store.Store
	agentID       int64
	cancelWaiters map[string]context.CancelFunc // workflowID → cancel func for Wait goroutines
}

func (s *wsAgentState) cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, cancel := range s.cancelWaiters {
		cancel()
	}
}

func (s *wsAgentState) agentCtx(ctx context.Context, hostname string) context.Context {
	md := grpcMetadata.Pairs(
		"agent_id", strconv.FormatInt(s.agentID, 10),
		"hostname", hostname,
	)
	return grpcMetadata.NewIncomingContext(ctx, md)
}

func (s *wsAgentState) handleMessage(ctx context.Context, env Envelope, hostname string) {
	switch env.Type {
	case MsgAgentRegister:
		s.handleRegister(ctx, env, hostname)
	case MsgAgentNext:
		s.handleNext(ctx, env, hostname)
	case MsgWorkflowInit:
		s.handleInit(ctx, env, hostname)
	case MsgWorkflowDone:
		s.handleDone(ctx, env, hostname)
	case MsgStepUpdate:
		s.handleUpdate(ctx, env, hostname)
	case MsgStepLog:
		s.handleLog(ctx, env, hostname)
	case MsgExtend:
		s.handleExtend(ctx, env, hostname)
	case MsgHealth:
		s.handleHealth(ctx, env, hostname)
	case MsgAgentUnregister:
		s.handleUnregister(ctx, env, hostname)
	default:
		sendAck(s.conn, env.Ref, fmt.Sprintf("unknown message type: %s", env.Type))
	}
}

func (s *wsAgentState) handleRegister(ctx context.Context, env Envelope, hostname string) {
	var p RegisterPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		sendAck(s.conn, env.Ref, "invalid register payload")
		return
	}

	agentCtx := s.agentCtx(ctx, hostname)
	agentID, err := s.rpc.RegisterAgent(agentCtx, rpc.AgentInfo{
		Platform:     p.Platform,
		Backend:      p.Backend,
		Capacity:     p.Capacity,
		Version:      p.Version,
		CustomLabels: p.CustomLabels,
	})
	if err != nil {
		sendAck(s.conn, env.Ref, err.Error())
		return
	}

	s.mu.Lock()
	s.agentID = agentID
	s.mu.Unlock()

	log.Info().Int64("agent_id", agentID).Str("hostname", hostname).Msg("ws-agent: registered")
	sendEnvelope(s.conn, MsgRegistered, env.Ref, RegisteredPayload{AgentID: agentID})
}

func (s *wsAgentState) handleNext(ctx context.Context, env Envelope, hostname string) {
	var p NextPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		sendAck(s.conn, env.Ref, "invalid next payload")
		return
	}

	// Run in goroutine — queue.Poll blocks until work available
	go func() {
		agentCtx := s.agentCtx(ctx, hostname)
		workflow, err := s.rpc.Next(agentCtx, rpc.Filter{Labels: p.FilterLabels})
		if err != nil {
			sendAck(s.conn, env.Ref, err.Error())
			return
		}
		if workflow == nil {
			// No work available (context canceled or shutdown)
			sendEnvelope(s.conn, MsgTaskAssign, env.Ref, nil)
			return
		}

		config, _ := json.Marshal(workflow.Config)
		sendEnvelope(s.conn, MsgTaskAssign, env.Ref, TaskAssignPayload{
			ID:      workflow.ID,
			Timeout: workflow.Timeout,
			Config:  config,
		})

		// Start Wait goroutine for this workflow — pushes task.cancel if server cancels
		s.startWaiter(ctx, workflow.ID, hostname)
	}()
}

func (s *wsAgentState) startWaiter(ctx context.Context, workflowID, hostname string) {
	waitCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.cancelWaiters[workflowID] = cancel
	s.mu.Unlock()

	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.cancelWaiters, workflowID)
			s.mu.Unlock()
		}()

		agentCtx := s.agentCtx(waitCtx, hostname)
		canceled, err := s.rpc.Wait(agentCtx, workflowID)
		if err != nil {
			log.Debug().Err(err).Str("workflow", workflowID).Msg("ws-agent: wait error")
			return
		}
		if canceled {
			sendEnvelope(s.conn, MsgTaskCancel, "", TaskCancelPayload{WorkflowID: workflowID})
		}
	}()
}

func (s *wsAgentState) handleInit(ctx context.Context, env Envelope, hostname string) {
	var p InitPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		sendAck(s.conn, env.Ref, "invalid init payload")
		return
	}

	agentCtx := s.agentCtx(ctx, hostname)
	err := s.rpc.Init(agentCtx, p.WorkflowID, rpc.WorkflowState{Started: p.Started})
	if err != nil {
		sendAck(s.conn, env.Ref, err.Error())
		return
	}
	sendAck(s.conn, env.Ref, "")
}

func (s *wsAgentState) handleDone(ctx context.Context, env Envelope, hostname string) {
	var p DonePayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		sendAck(s.conn, env.Ref, "invalid done payload")
		return
	}

	// Cancel the waiter for this workflow
	s.mu.Lock()
	if cancel, ok := s.cancelWaiters[p.WorkflowID]; ok {
		cancel()
	}
	s.mu.Unlock()

	agentCtx := s.agentCtx(ctx, hostname)
	err := s.rpc.Done(agentCtx, p.WorkflowID, rpc.WorkflowState{
		Started:  p.Started,
		Finished: p.Finished,
		Error:    p.Error,
		Canceled: p.Canceled,
	})
	if err != nil {
		sendAck(s.conn, env.Ref, err.Error())
		return
	}
	sendAck(s.conn, env.Ref, "")
}

func (s *wsAgentState) handleUpdate(ctx context.Context, env Envelope, hostname string) {
	var p UpdatePayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		sendAck(s.conn, env.Ref, "invalid update payload")
		return
	}

	agentCtx := s.agentCtx(ctx, hostname)
	err := s.rpc.Update(agentCtx, p.WorkflowID, rpc.StepState{
		StepUUID: p.StepUUID,
		Started:  p.Started,
		Finished: p.Finished,
		Exited:   p.Exited,
		ExitCode: p.ExitCode,
		Error:    p.Error,
		Canceled: p.Canceled,
	})
	if err != nil {
		sendAck(s.conn, env.Ref, err.Error())
		return
	}
	sendAck(s.conn, env.Ref, "")
}

func (s *wsAgentState) handleLog(ctx context.Context, env Envelope, hostname string) {
	var p LogPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		sendAck(s.conn, env.Ref, "invalid log payload")
		return
	}

	entries := make([]*rpc.LogEntry, len(p.Entries))
	for i, e := range p.Entries {
		entries[i] = &rpc.LogEntry{
			StepUUID: p.StepUUID,
			Time:     e.Time,
			Type:     e.Type,
			Line:     e.Line,
			Data:     e.Data,
		}
	}

	agentCtx := s.agentCtx(ctx, hostname)
	if err := s.rpc.Log(agentCtx, p.StepUUID, entries); err != nil {
		log.Debug().Err(err).Msg("ws-agent: log write failed")
	}
	// No ack for logs — fire and forget (same as gRPC pattern)
}

func (s *wsAgentState) handleExtend(ctx context.Context, env Envelope, hostname string) {
	var p ExtendPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return
	}

	agentCtx := s.agentCtx(ctx, hostname)
	for _, wfID := range p.WorkflowIDs {
		if err := s.rpc.Extend(agentCtx, wfID); err != nil {
			log.Debug().Err(err).Str("workflow", wfID).Msg("ws-agent: extend failed")
		}
	}
}

func (s *wsAgentState) handleHealth(ctx context.Context, env Envelope, hostname string) {
	// Extend leases for running workflows (piggybacked on health)
	var p HealthPayload
	_ = json.Unmarshal(env.Payload, &p)

	agentCtx := s.agentCtx(ctx, hostname)
	for _, wfID := range p.WorkflowIDs {
		_ = s.rpc.Extend(agentCtx, wfID)
	}

	// Update agent last contact
	if s.store != nil && s.agentID > 0 {
		if agent, err := s.store.AgentFind(s.agentID); err == nil {
			agent.LastContact = time.Now().Unix()
			_ = s.store.AgentUpdate(agent)
		}
	}

	_ = s.rpc.ReportHealth(agentCtx, "I am alive!")
}

func (s *wsAgentState) handleUnregister(ctx context.Context, env Envelope, hostname string) {
	agentCtx := s.agentCtx(ctx, hostname)
	_ = s.rpc.UnregisterAgent(agentCtx)
	sendAck(s.conn, env.Ref, "")
	log.Info().Str("hostname", hostname).Msg("ws-agent: unregistered")
}

// sendEnvelope marshals and sends an envelope.
func sendEnvelope(conn *websocket.Conn, msgType, ref string, payload interface{}) {
	data, err := newEnvelope(msgType, ref, payload)
	if err != nil {
		log.Error().Err(err).Str("type", msgType).Msg("ws-agent: marshal failed")
		return
	}
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		log.Debug().Err(err).Str("type", msgType).Msg("ws-agent: send failed")
	}
}

func sendAck(conn *websocket.Conn, ref, errMsg string) {
	ok := errMsg == ""
	sendEnvelope(conn, MsgAck, ref, AckPayload{OK: ok, Error: errMsg})
}
