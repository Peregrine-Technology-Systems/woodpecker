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
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog/log"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	grpcserver "go.woodpecker-ci.org/woodpecker/v3/server/rpc"
	"go.woodpecker-ci.org/woodpecker/v3/server/store"
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
// dispatch tick, whether a running task's owning agent has been POSITIVELY
// observed as disconnected. If so, the queue triggers ReclaimAgentTasks within
// one tick instead of waiting the lease.
//
// #246 — fail SAFE, not fail DANGEROUS. The reclaim keys on a positively-
// observed disconnect (IsAgentKnownDisconnected), NOT on "absent from the
// connected set." Only the WS transport (server/api/ws_agent.go) populates the
// connected set; agents on the gRPC transport (WOODPECKER_AGENT_TRANSPORT=grpc,
// the default — e.g. the co-located d3ci42-local backend:local agent) never
// register through that path. The pre-#246 oracle treated those gRPC agents as
// "not connected" ⇒ dead, and ReleaseAgentTasks killed their clone step at
// t+0s, jamming every tier:local pipeline. With a known-disconnected oracle an
// untracked agent is treated as ALIVE and falls back to the TaskTimeout lease,
// exactly as before #244 — only an agent whose WS reconnect grace expired is
// ever reclaimed early.

var (
	connectedAgentsMu sync.RWMutex
	connectedAgents   = make(map[int64]struct{})
	// disconnectedAgents holds agents POSITIVELY observed as gone: their #208
	// WS reconnect grace expired without a re-register. This is the only set the
	// reclaim path keys on (#246). Entries are dropped on reconnect
	// (markAgentConnected) and after a reclaim releases the agent's tasks
	// (forgetDisconnectedAgent), so the set tracks only currently-dead WS agents.
	disconnectedAgents = make(map[int64]struct{})
)

// markAgentConnected records agentID as having a live connection. Called from
// handleRegister, so a reconnect (even under the same id within the #208 grace
// window) re-asserts liveness and clears any prior known-disconnected mark.
func markAgentConnected(agentID int64) {
	if agentID <= 0 {
		return
	}
	connectedAgentsMu.Lock()
	connectedAgents[agentID] = struct{}{}
	delete(disconnectedAgents, agentID)
	connectedAgentsMu.Unlock()
}

// markAgentDisconnected records agentID as positively gone. Called ONLY when
// the #208 reconnect grace expires (alongside ReleaseAgentTasks) — never on the
// raw WS close — so a transient blip that reconnects within the grace keeps the
// agent connected and its in-flight task is never reclaimed early.
func markAgentDisconnected(agentID int64) {
	if agentID <= 0 {
		return
	}
	connectedAgentsMu.Lock()
	delete(connectedAgents, agentID)
	disconnectedAgents[agentID] = struct{}{}
	connectedAgentsMu.Unlock()
}

// forgetDisconnectedAgent drops agentID from the known-disconnected set after
// its tasks have been reclaimed, bounding the set to currently-dead agents.
func forgetDisconnectedAgent(agentID int64) {
	connectedAgentsMu.Lock()
	delete(disconnectedAgents, agentID)
	connectedAgentsMu.Unlock()
}

// IsAgentConnected reports whether agentID currently has a live WS connection.
func IsAgentConnected(agentID int64) bool {
	connectedAgentsMu.RLock()
	_, ok := connectedAgents[agentID]
	connectedAgentsMu.RUnlock()
	return ok
}

// IsAgentKnownDisconnected reports whether agentID has been POSITIVELY observed
// as disconnected (its WS reconnect grace expired). This is the queue's
// fail-safe reclaim oracle (#246): an agent never tracked through the WS path
// returns false and is never reclaimed early. Wired in cmd/server/setup.go.
func IsAgentKnownDisconnected(agentID int64) bool {
	connectedAgentsMu.RLock()
	_, ok := disconnectedAgents[agentID]
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

	// #246: the agent's tasks are now released — drop it from the known-
	// disconnected set so the gauge clears and the set stays bounded to
	// currently-dead agents. A reconnect under the same id would re-add via
	// markAgentConnected; a recycle under a new id (#77) never returns here.
	forgetDisconnectedAgent(agentID)
}

// AgentStaleThreshold is how long an agent may go without refreshing
// LastContact before IsAgentLastContactStale reports it stale. Both transports
// stamp LastContact on every health beat (gRPC ReportHealth, WS handleHealth),
// which agents send every reportHealthInterval (10s). Set to 9 missed beats so
// a momentarily quiet agent is never flagged — fail-safe per #248. Var (not
// const) so tests can shrink it.
var AgentStaleThreshold = 90 * time.Second

// IsAgentLastContactStale reports whether an agent's last health beat is older
// than AgentStaleThreshold. This is the transport-agnostic liveness signal
// #248 asked for: unlike IsAgentKnownDisconnected (populated only by the WS
// reconnect-grace path), LastContact is refreshed by BOTH gRPC and WS health
// beats, so this covers gRPC/local-backend agents the WS-only known-dead
// registry never sees — the previously-invisible "running task stranded on a
// dead agent" manifestation (B).
//
// It is OBSERVE-ONLY: it feeds the running_owner_stale gauge so a recurrence is
// measurable; it never drives a reclaim (a reclaim on mere LastContact aging
// would re-introduce the t+0s kill #246 fixed). Fails safe — an unknown agent,
// a store error, or LastContact==0 (never reported) returns false, so nothing
// is ever counted as stranded on weak evidence.
func IsAgentLastContactStale(agentID int64, s store.Store) bool {
	if agentID <= 0 || s == nil {
		return false
	}
	agent, err := s.AgentFind(agentID)
	if err != nil || agent == nil || agent.LastContact <= 0 {
		return false
	}
	return time.Since(time.Unix(agent.LastContact, 0)) > AgentStaleThreshold
}

// agentsReapedTotal counts orphan agent registrations deleted by the #254
// reaper — alert on a sustained non-zero rate (it means agents keep dying
// without unregistering; the dominant fixable source is a scaler scale-down
// without graceful drain, peregrine-ci-scaler#1426).
var agentsReapedTotal = promauto.NewCounter(prometheus.CounterOpts{
	Namespace: "woodpecker",
	Name:      "agents_reaped_total",
	Help:      "Orphan agent registrations deleted by the last_contact reaper (#254).",
})

// AgentReapThreshold is how long an agent may go without refreshing LastContact
// before the orphan-agent reaper (ReapOrphanAgents, #254) deletes its
// registration row. Far more generous than AgentStaleThreshold (which only
// drives an observe-only gauge): deleting a row is irreversible, so the window
// is set past shared/constant.TaskTimeout (15m) — a reaped agent's tasks are
// already released/timed-out, so it owns nothing live. Staleness past this
// window IS the protection: any agent that is up — including the persistent
// d3ci42-local box — refreshes LastContact every health beat (~10s) and is
// therefore NEVER reapable, so no name-based carve-out is needed. Var (not
// const) so tests can shrink it.
var AgentReapThreshold = 30 * time.Minute

// agentReapableAt reports whether an agent's registration row is an orphan safe
// to delete at time `now`. All three must hold:
//   - it is a SYSTEM agent (auto-registered via the shared agent token,
//     OwnerID==IDNotSet) — mirrors UnregisterAgent, which likewise never deletes
//     an individually-tokened, pre-provisioned agent. This is the explicit
//     protection for such agents, independent of staleness;
//   - it has reported at least once (LastContact > 0) — a never-reported row
//     (e.g. mid-registration) is left alone, fail-safe;
//   - its last health beat is older than AgentReapThreshold.
//
// The staleness gate is a second, independent protection: any live agent
// (including a system-token d3ci42-local) heartbeats every ~10s and is never
// stale, so it is never reaped even though it is a system agent. Pure, for
// testability.
func agentReapableAt(agent *model.Agent, now time.Time) bool {
	if agent == nil || !agent.IsSystemAgent() || agent.LastContact <= 0 {
		return false
	}
	return now.Sub(time.Unix(agent.LastContact, 0)) > AgentReapThreshold
}

// ReapOrphanAgents deletes the registration rows of agents whose LastContact is
// stale past AgentReapThreshold — the #254 durable, transport-agnostic backstop
// for agents that died WITHOUT unregistering: ungraceful spot preemption / OOM
// / crash on any transport (the irreducible floor), plus deaths the in-memory
// WS disconnect set lost to a server restart. It is the GC for the missing
// registration lease — reconciliation against LastContact (source of truth),
// not suppression. Returns the count reaped. Best-effort: a per-agent delete
// error is logged and skipped, never aborts the sweep. Wired to an in-process
// ticker in cmd/server/setup.go (NOT a Woodpecker cron — global rule #11).
func ReapOrphanAgents(s store.Store) int {
	if s == nil {
		return 0
	}
	agents, err := s.AgentList(&model.ListOptions{All: true})
	if err != nil {
		log.Error().Err(err).Msg("agent-reaper: AgentList failed")
		return 0
	}
	now := time.Now()
	reaped := 0
	for _, a := range agents {
		if !agentReapableAt(a, now) {
			continue
		}
		if err := s.AgentDelete(a); err != nil {
			log.Warn().Err(err).Int64("agent_id", a.ID).Str("name", a.Name).
				Msg("agent-reaper: failed to delete orphan agent registration")
			continue
		}
		reaped++
		agentsReapedTotal.Inc()
		forgetDisconnectedAgent(a.ID) // keep the in-memory known-dead set bounded
		log.Info().Int64("agent_id", a.ID).Str("name", a.Name).Int64("last_contact", a.LastContact).
			Msg("agent-reaper: deleted orphan agent registration (#254)")
	}
	if reaped > 0 {
		log.Info().Int("reaped", reaped).Int("scanned", len(agents)).Msg("agent-reaper: sweep complete")
	}
	return reaped
}
