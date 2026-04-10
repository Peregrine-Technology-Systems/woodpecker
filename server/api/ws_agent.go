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
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
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

	// Connection-scoped context — lives for the entire WS connection.
	// Do NOT use c.Request.Context() — gin may cancel it unexpectedly,
	// killing queue.Poll goroutines mid-flight.
	connCtx, connCancel := context.WithCancel(context.Background())
	defer connCancel()

	// Agent state
	state := &wsAgentState{
		conn:          conn,
		rpc:           rpcPeer,
		store:         _store,
		cancelWaiters: make(map[string]context.CancelFunc),
	}

	// Send version on connect
	state.send(MsgVersion, "", VersionPayload{
		ServerVersion: version.String(),
	})

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
			state.sendAck("", "invalid envelope")
			continue
		}

		state.handleMessage(connCtx, env, hostname)
	}
}

// wsAgentState tracks per-connection state for one agent.
type wsAgentState struct {
	mu            sync.Mutex
	writeMu       sync.Mutex // protects WebSocket writes — gorilla/websocket is not thread-safe
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
		s.sendAck(env.Ref, fmt.Sprintf("unknown message type: %s", env.Type))
	}
}

func (s *wsAgentState) handleRegister(_ context.Context, env Envelope, hostname string) {
	var p RegisterPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		s.sendAck(env.Ref, "invalid register payload")
		return
	}

	if s.store == nil {
		s.sendAck(env.Ref, "store not available")
		return
	}

	// Create or find system agent (same logic as gRPC auth_server.go)
	agent := &model.Agent{
		Name:         hostname,
		OwnerID:      model.IDNotSet, // system agent — not owned by a user
		OrgID:        model.IDNotSet, // global agent — can serve all orgs
		Backend:      p.Backend,
		Platform:     p.Platform,
		Capacity:     int32(p.Capacity),
		Version:      p.Version,
		CustomLabels: p.CustomLabels,
		LastContact:  time.Now().Unix(),
		Token:        server.Config.Server.AgentToken,
	}
	if err := s.store.AgentCreate(agent); err != nil {
		// Agent may already exist — try to find by name
		agents, listErr := s.store.AgentList(&model.ListOptions{All: true})
		if listErr != nil {
			s.sendAck(env.Ref, err.Error())
			return
		}
		found := false
		for _, a := range agents {
			if a.Name == hostname {
				agent = a
				agent.Backend = p.Backend
				agent.Platform = p.Platform
				agent.Capacity = int32(p.Capacity)
				agent.Version = p.Version
				agent.CustomLabels = p.CustomLabels
				agent.LastContact = time.Now().Unix()
				_ = s.store.AgentUpdate(agent)
				found = true
				break
			}
		}
		if !found {
			s.sendAck(env.Ref, fmt.Sprintf("register failed: %v", err))
			return
		}
	}

	s.mu.Lock()
	s.agentID = agent.ID
	s.mu.Unlock()

	log.Info().Int64("agent_id", agent.ID).Str("hostname", hostname).Msg("ws-agent: registered")
	s.send(MsgRegistered, env.Ref, RegisteredPayload{AgentID: agent.ID})
}

func (s *wsAgentState) handleNext(ctx context.Context, env Envelope, hostname string) {
	var p NextPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		s.sendAck(env.Ref, "invalid next payload")
		return
	}

	// Guard: agent must be registered before polling (#891)
	if s.agentID == 0 {
		log.Debug().Str("hostname", hostname).Msg("ws-agent: Next() skipped — agent not registered")
		s.sendAck(env.Ref, "agent not registered")
		return
	}

	// Run in goroutine — queue.Poll blocks until work available
	go func() {
		log.Debug().Int64("agent_id", s.agentID).Str("hostname", hostname).
			Any("filter", p.FilterLabels).Msg("ws-agent: polling for work")
		agentCtx := s.agentCtx(ctx, hostname)
		workflow, err := s.rpc.Next(agentCtx, rpc.Filter{Labels: p.FilterLabels})
		if err != nil {
			if ctx.Err() != nil {
				// Connection closed — normal during disconnect, not an error
				return
			}
			log.Error().Err(err).Int64("agent_id", s.agentID).Msg("ws-agent: Next() error")
			s.sendAck(env.Ref, err.Error())
			return
		}
		if workflow == nil {
			// No work available (context canceled or shutdown)
			s.send(MsgTaskAssign, env.Ref, nil)
			return
		}

		config, _ := json.Marshal(workflow.Config)
		s.send(MsgTaskAssign, env.Ref, TaskAssignPayload{
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
			s.send(MsgTaskCancel, "", TaskCancelPayload{WorkflowID: workflowID})
		}
	}()
}

func (s *wsAgentState) handleInit(ctx context.Context, env Envelope, hostname string) {
	var p InitPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		s.sendAck(env.Ref, "invalid init payload")
		return
	}

	agentCtx := s.agentCtx(ctx, hostname)
	err := s.rpc.Init(agentCtx, p.WorkflowID, rpc.WorkflowState{Started: p.Started})
	if err != nil {
		s.sendAck(env.Ref, err.Error())
		return
	}
	s.sendAck(env.Ref, "")
}

func (s *wsAgentState) handleDone(ctx context.Context, env Envelope, hostname string) {
	var p DonePayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		s.sendAck(env.Ref, "invalid done payload")
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
		s.sendAck(env.Ref, err.Error())
		return
	}
	s.sendAck(env.Ref, "")
}

func (s *wsAgentState) handleUpdate(ctx context.Context, env Envelope, hostname string) {
	var p UpdatePayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		s.sendAck(env.Ref, "invalid update payload")
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
		s.sendAck(env.Ref, err.Error())
		return
	}
	s.sendAck(env.Ref, "")
}

func (s *wsAgentState) handleLog(ctx context.Context, env Envelope, hostname string) {
	var p LogPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		s.sendAck(env.Ref, "invalid log payload")
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
	s.sendAck(env.Ref, "")
	log.Info().Str("hostname", hostname).Msg("ws-agent: unregistered")
}

// send marshals and sends an envelope with write mutex protection.
func (s *wsAgentState) send(msgType, ref string, payload interface{}) {
	data, err := newEnvelope(msgType, ref, payload)
	if err != nil {
		log.Error().Err(err).Str("type", msgType).Msg("ws-agent: marshal failed")
		return
	}
	s.writeMu.Lock()
	err = s.conn.WriteMessage(websocket.TextMessage, data)
	s.writeMu.Unlock()
	if err != nil {
		log.Debug().Err(err).Str("type", msgType).Msg("ws-agent: send failed")
	}
}

func (s *wsAgentState) sendAck(ref, errMsg string) {
	ok := errMsg == ""
	s.send(MsgAck, ref, AckPayload{OK: ok, Error: errMsg})
}
