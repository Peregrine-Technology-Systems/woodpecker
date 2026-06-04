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

	grpcserver "go.woodpecker-ci.org/woodpecker/v3/server/rpc"
)

// #208: Server-side WebSocket reconnect grace window.
//
// Without this, every WS disconnect immediately calls
// ReleaseAgentTasks → workflows are killed. Forensics on #203 showed
// that ~10 task-bearing disconnects/day (matching peregrine-grafana#190's
// mode-C sample) are transient — the agent reconnects within seconds.
//
// With this in place, cleanup() schedules ReleaseAgentTasks to fire
// after `wsReconnectGrace`. If the SAME agent_id reconnects (and
// re-registers via handleRegister) within that window, the scheduled
// release is canceled — the agent's in-flight workflows survive the
// blip.
//
// The grace window is bounded so a TRULY dead agent (never coming back)
// still releases its tasks. We choose 30s because the agent's existing
// reconnect logic uses 5-30s exponential backoff (see
// agent/rpc/client_ws.go::ensureConnected).
//
// Variable rather than const so tests can shrink the window to
// millisecond scale.
var wsReconnectGrace = 30 * time.Second

var (
	pendingReleasesMu sync.Mutex
	pendingReleases   = make(map[int64]*time.Timer)

	// woodpecker_ws_reconnect_total counts the outcome of every grace
	// window. `success` = agent reconnected before grace expired (task
	// preserved). `abandoned` = grace expired without reconnect, tasks
	// released. The ratio is a key SLI for #190 mode-C.
	wsReconnectTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "woodpecker",
		Name:      "ws_reconnect_total",
		Help:      "Outcome of the WebSocket reconnect grace window: success (agent came back) vs abandoned (grace expired, tasks released) (#208).",
	}, []string{"outcome"})
)

// scheduleAgentRelease arms a deferred ReleaseAgentTasks for agentID.
// If agentID already has a pending release scheduled, it is canceled
// first (shouldn't happen in normal flow but defensive). The release
// fires after wsReconnectGrace unless cancelPendingAgentRelease is
// called for the same agent_id before then.
//
// Caller must NOT hold any wsAgentState locks.
func scheduleAgentRelease(agentID int64, rpcPeer *grpcserver.RPC) {
	pendingReleasesMu.Lock()
	defer pendingReleasesMu.Unlock()

	if prior := pendingReleases[agentID]; prior != nil {
		prior.Stop()
	}

	log.Info().Int64("agent_id", agentID).Dur("grace", wsReconnectGrace).
		Msg("ws-agent: scheduling deferred release after disconnect")

	pendingReleases[agentID] = time.AfterFunc(wsReconnectGrace, func() {
		pendingReleasesMu.Lock()
		delete(pendingReleases, agentID)
		pendingReleasesMu.Unlock()
		onReconnectGraceExpired(agentID, rpcPeer)
	})
}

// onReconnectGraceExpired runs when an agent fails to reconnect within the #208
// grace window. The agent is positively gone: it (1) is dropped from the
// connected set so the queue's owner-liveness reclaim treats any task still (or
// newly) stranded on this id as orphaned (#243), (2) has its tasks released, and
// (3) — the #283 fast-path — has its now-orphan registration row removed, unless
// a re-register raced in after the grace fired (IsAgentConnected), in which case
// the fresh row is kept. RemoveAgent is best-effort and only deletes system
// agents, so a failure or a pre-provisioned agent simply falls through to the
// #254 last_contact reaper. Extracted from the AfterFunc so every branch is
// directly testable without driving the timer.
func onReconnectGraceExpired(agentID int64, rpcPeer *grpcserver.RPC) {
	log.Warn().Int64("agent_id", agentID).
		Msg("ws-agent: reconnect grace expired, releasing tasks")
	wsReconnectTotal.WithLabelValues("abandoned").Inc()
	markAgentDisconnected(agentID)
	rpcPeer.ReleaseAgentTasks(context.Background(), agentID)
	removeAgentIfStillGone(agentID, rpcPeer)
}

// removeAgentIfStillGone is the #283 fast-path: delete the now-orphan
// registration row of an agent whose grace expired. Skips the delete if a
// re-register raced in after the grace fired (IsAgentConnected) — that fresh row
// must be kept. RemoveAgent is best-effort and only deletes system agents, so a
// failure or a pre-provisioned agent falls through to the #254 reaper. Separate
// function so both the skip and the delete branches are deterministically
// testable (within onReconnectGraceExpired, markAgentDisconnected has already
// cleared the connected set, so only a concurrent reconnect re-sets it).
func removeAgentIfStillGone(agentID int64, rpcPeer *grpcserver.RPC) {
	if IsAgentConnected(agentID) {
		return
	}
	if err := rpcPeer.RemoveAgent(agentID); err != nil {
		log.Warn().Err(err).Int64("agent_id", agentID).
			Msg("ws-agent: failed to remove registration on disconnect (#254 reaper will retry)")
	}
}

// cancelPendingAgentRelease stops a pending release for agentID, if
// one is scheduled. Returns true if a release was canceled (i.e. the
// agent reconnected within the grace window). Increments the
// `success` outcome counter.
//
// Caller must NOT hold any wsAgentState locks.
func cancelPendingAgentRelease(agentID int64) bool {
	pendingReleasesMu.Lock()
	defer pendingReleasesMu.Unlock()

	timer := pendingReleases[agentID]
	if timer == nil {
		return false
	}
	timer.Stop()
	delete(pendingReleases, agentID)

	log.Info().Int64("agent_id", agentID).
		Msg("ws-agent: agent reconnected within grace, canceling deferred release")
	wsReconnectTotal.WithLabelValues("success").Inc()
	return true
}
