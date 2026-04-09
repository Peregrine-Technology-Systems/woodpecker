package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog/log"

	"go.woodpecker-ci.org/woodpecker/v3/server"
	"go.woodpecker-ci.org/woodpecker/v3/server/store"
)

// HeartbeatMessage is sent by agents every 10s over WebSocket.
type HeartbeatMessage struct {
	Hostname    string   `json:"hostname"`
	AgentID     int64    `json:"agent_id"`
	WorkflowIDs []string `json:"workflow_ids"`
}

// HeartbeatAck is sent back to the agent after processing.
type HeartbeatAck struct {
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
	Extended int    `json:"extended"`
}

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(_ *http.Request) bool { return true },
}

// WSHeartbeat handles WebSocket connections from agents for lease extension.
// Phase 0 (#860): agents send heartbeat every 10s with running workflow IDs.
// Server extends queue leases, bypassing fragile gRPC Extend path.
func WSHeartbeat(c *gin.Context) {
	token := c.Query("token")
	if token == "" || token != server.Config.Server.AgentToken {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
		return
	}

	conn, err := wsUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Error().Err(err).Msg("ws-heartbeat: upgrade failed")
		return
	}
	defer conn.Close()

	log.Debug().Msg("ws-heartbeat: agent connected")

	_store := store.FromContext(c)

	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err,
				websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				log.Warn().Err(err).Msg("ws-heartbeat: unexpected close")
			}
			return
		}

		var hb HeartbeatMessage
		if err := json.Unmarshal(message, &hb); err != nil {
			writeAck(conn, HeartbeatAck{Error: "invalid message"})
			continue
		}

		if hb.AgentID <= 0 {
			writeAck(conn, HeartbeatAck{Error: "missing agent_id"})
			continue
		}

		// Extend queue leases for all running workflows
		extended := 0
		q := server.Config.Services.Queue
		for _, wfID := range hb.WorkflowIDs {
			if err := q.Extend(c.Request.Context(), hb.AgentID, wfID); err != nil {
				log.Debug().Err(err).Str("workflow", wfID).
					Msg("ws-heartbeat: extend failed")
			} else {
				extended++
			}
		}

		// Update agent last_contact so Woodpecker knows the agent is alive
		if _store != nil && hb.AgentID > 0 {
			if agent, err := _store.AgentFind(hb.AgentID); err == nil {
				agent.LastContact = time.Now().Unix()
				_ = _store.AgentUpdate(agent)
			}
		}

		writeAck(conn, HeartbeatAck{OK: true, Extended: extended})
	}
}

func writeAck(conn *websocket.Conn, ack HeartbeatAck) {
	data, _ := json.Marshal(ack)
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		log.Debug().Err(err).Msg("ws-heartbeat: write ack failed")
	}
}
