// Copyright 2026 Woodpecker Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package api

import (
	"context"
	"sync"

	grpcserver "go.woodpecker-ci.org/woodpecker/v3/server/rpc"
)

// #243: connected-agent registry + owner-liveness reclaim.
//
// The dispatcher's only signal that a running task's agent died used to be
// the 15-minute TaskTimeout lease (shared/constant.TaskTimeout) — a task
// stranded on a recycled agent sat in q.running for the full window before
// resubmitExpiredPipelines re-queued it. That window is the gap behind #243:
// a d3ci42-local agent recycled mid-pipeline and came back under a NEW
// agent_id (#77), so the deferred ReleaseAgentTasks (#208) for the OLD id
// reclaimed nothing, and the straddler workflow never re-dispatched.
//
// This registry lets the queue (server/queue/fifo.go) ask, every ~100ms
// dispatch tick, whether a running task's owning agent is still connected.
// If not, the queue triggers ReclaimAgentTasks within one tick instead of
// waiting the lease. Liveness is "has a live WS connection," NOT "is idle,"
// so a busy agent running a quiet step is never mistaken for dead.

var (
	connectedAgentsMu sync.RWMutex
	connectedAgents   = make(map[int64]struct{})
)

// markAgentConnected records agentID as having a live connection. Called from
// handleRegister, so a reconnect (even under the same id within the #208 grace
// window) re-asserts liveness.
func markAgentConnected(agentID int64) {
	if agentID <= 0 {
		return
	}
	connectedAgentsMu.Lock()
	connectedAgents[agentID] = struct{}{}
	connectedAgentsMu.Unlock()
}

// markAgentDisconnected removes agentID from the connected set. Called ONLY
// when the #208 reconnect grace expires (alongside ReleaseAgentTasks) — never
// on the raw WS close — so a transient blip that reconnects within the grace
// keeps the agent "connected" and its in-flight task is never reclaimed early.
func markAgentDisconnected(agentID int64) {
	connectedAgentsMu.Lock()
	delete(connectedAgents, agentID)
	connectedAgentsMu.Unlock()
}

// IsAgentConnected reports whether agentID currently has a live connection.
// Wired into the queue liveness oracle in cmd/server/setup.go.
func IsAgentConnected(agentID int64) bool {
	connectedAgentsMu.RLock()
	_, ok := connectedAgents[agentID]
	connectedAgentsMu.RUnlock()
	return ok
}

// reclaimInFlight dedupes ReleaseAgentTasks for the same agent. The queue tick
// can observe a stranded task across several consecutive ~100ms ticks before
// ReleaseAgentTasks (store I/O) finishes; without this guard it would stack
// redundant reclaims for one agent.
var (
	reclaimInFlightMu sync.Mutex
	reclaimInFlight   = make(map[int64]struct{})
)

// ReclaimAgentTasks is the queue's owner-liveness reclaim callback. It runs
// ReleaseAgentTasks for a disconnected agent that still owns a running task,
// reusing that function's safe re-queue-vs-kill partition: a step that did
// real work is killed (never blindly re-run, so a partial deploy is not
// duplicated), and a claimed-but-no-work task is re-queued for the next agent.
// Deduped per agent. Safe to call from a goroutine. (#243)
func ReclaimAgentTasks(agentID int64, rpcPeer *grpcserver.RPC) {
	if rpcPeer == nil || agentID <= 0 {
		return
	}

	reclaimInFlightMu.Lock()
	if _, busy := reclaimInFlight[agentID]; busy {
		reclaimInFlightMu.Unlock()
		return
	}
	reclaimInFlight[agentID] = struct{}{}
	reclaimInFlightMu.Unlock()

	defer func() {
		reclaimInFlightMu.Lock()
		delete(reclaimInFlight, agentID)
		reclaimInFlightMu.Unlock()
	}()

	rpcPeer.ReleaseAgentTasks(context.Background(), agentID)
}
