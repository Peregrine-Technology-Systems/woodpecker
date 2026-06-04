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
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/queue"
	queue_mocks "go.woodpecker-ci.org/woodpecker/v3/server/queue/mocks"
	grpcserver "go.woodpecker-ci.org/woodpecker/v3/server/rpc"
	store_mocks "go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
)

func TestMarkAndQueryAgentConnected(t *testing.T) {
	const id int64 = 5150
	t.Cleanup(func() { forgetDisconnectedAgent(id) })

	assert.False(t, IsAgentConnected(id), "unknown agent is not connected")

	markAgentConnected(id)
	assert.True(t, IsAgentConnected(id))

	markAgentDisconnected(id)
	assert.False(t, IsAgentConnected(id), "after disconnect it's gone again")
}

func TestMarkAgentConnected_IgnoresNonPositiveID(t *testing.T) {
	markAgentConnected(0)
	markAgentConnected(-1)
	assert.False(t, IsAgentConnected(0))
	assert.False(t, IsAgentConnected(-1))
}

// TestIsAgentKnownDisconnected_FailSafe is the #246 guard at the registry
// layer: an agent the WS path never observed disconnecting is NOT known-dead,
// so the queue's reclaim oracle returns false and the agent's tasks are left to
// the TaskTimeout lease. markAgentDisconnected (only the #208 grace expiry calls
// it) is the sole producer of the known-disconnected state; reconnect clears it.
func TestIsAgentKnownDisconnected_FailSafe(t *testing.T) {
	const id int64 = 23000 // a gRPC/local-backend agent id, never WS-tracked
	t.Cleanup(func() { forgetDisconnectedAgent(id) })

	assert.False(t, IsAgentKnownDisconnected(id), "untracked agent must not read as dead (#246)")

	markAgentConnected(id)
	assert.False(t, IsAgentKnownDisconnected(id), "a connected agent is not known-dead")

	markAgentDisconnected(id)
	assert.True(t, IsAgentKnownDisconnected(id), "grace-expiry positively marks it dead")
	assert.False(t, IsAgentConnected(id))

	markAgentConnected(id)
	assert.False(t, IsAgentKnownDisconnected(id), "a reconnect clears the known-dead mark")
}

func TestMarkAgentDisconnected_IgnoresNonPositiveID(t *testing.T) {
	markAgentDisconnected(0)
	markAgentDisconnected(-1)
	assert.False(t, IsAgentKnownDisconnected(0))
	assert.False(t, IsAgentKnownDisconnected(-1))
}

// TestIsAgentLastContactStale exercises the transport-agnostic, observe-only
// liveness oracle (#248) across all branches. It must fail SAFE: only a known
// agent with a positive LastContact aged past the threshold reads as stale;
// every weaker signal (bad id, nil store, store error, never-contacted,
// recent contact) returns false so nothing is ever counted on weak evidence.
func TestIsAgentLastContactStale(t *testing.T) {
	orig := AgentStaleThreshold
	AgentStaleThreshold = 90 * time.Second
	t.Cleanup(func() { AgentStaleThreshold = orig })

	t.Run("non-positive id is never stale", func(t *testing.T) {
		assert.False(t, IsAgentLastContactStale(0, store_mocks.NewMockStore(t)))
		assert.False(t, IsAgentLastContactStale(-1, store_mocks.NewMockStore(t)))
	})

	t.Run("nil store is never stale", func(t *testing.T) {
		assert.False(t, IsAgentLastContactStale(5, nil))
	})

	t.Run("store error fails safe", func(t *testing.T) {
		s := store_mocks.NewMockStore(t)
		s.On("AgentFind", int64(5)).Return((*model.Agent)(nil), errors.New("boom"))
		assert.False(t, IsAgentLastContactStale(5, s))
	})

	t.Run("never-contacted agent (LastContact 0) is not stale", func(t *testing.T) {
		s := store_mocks.NewMockStore(t)
		s.On("AgentFind", int64(5)).Return(&model.Agent{ID: 5, LastContact: 0}, nil)
		assert.False(t, IsAgentLastContactStale(5, s))
	})

	t.Run("recent contact is not stale", func(t *testing.T) {
		s := store_mocks.NewMockStore(t)
		s.On("AgentFind", int64(5)).Return(&model.Agent{ID: 5, LastContact: time.Now().Unix()}, nil)
		assert.False(t, IsAgentLastContactStale(5, s))
	})

	t.Run("contact aged past threshold is stale", func(t *testing.T) {
		s := store_mocks.NewMockStore(t)
		aged := time.Now().Add(-2 * AgentStaleThreshold).Unix()
		s.On("AgentFind", int64(5)).Return(&model.Agent{ID: 5, LastContact: aged}, nil)
		assert.True(t, IsAgentLastContactStale(5, s))
	})
}

func TestReclaimAgentTasks_NoOpGuards(t *testing.T) {
	// nil peer and non-positive id are safe no-ops (no panic, no reclaim state).
	ReclaimAgentTasks(7, nil)
	ReclaimAgentTasks(0, nil)

	reclaimInFlightMu.Lock()
	_, busy := reclaimInFlight[7]
	reclaimInFlightMu.Unlock()
	assert.False(t, busy, "guarded no-op must not leave in-flight state")
}

func TestReclaimAgentTasks_InvokesReleaseAndClearsGuard(t *testing.T) {
	q := queue_mocks.NewMockQueue(t)
	// ReleaseAgentTasks starts by snapshotting the queue; empty Info means no
	// orphaned tasks, so it returns after the single Info call.
	q.On("Info", mock.Anything).Return(queue.InfoT{}).Once()
	rpcPeer := grpcserver.NewRPCForTesting(q, nil)

	// Seed the known-disconnected mark so we can assert the reclaim clears it.
	markAgentDisconnected(99)
	t.Cleanup(func() { forgetDisconnectedAgent(99) })

	ReclaimAgentTasks(99, rpcPeer)

	reclaimInFlightMu.Lock()
	_, busy := reclaimInFlight[99]
	reclaimInFlightMu.Unlock()
	assert.False(t, busy, "guard must be released after reclaim returns")
	assert.False(t, IsAgentKnownDisconnected(99),
		"reclaim forgets the agent so the gauge clears and the set stays bounded (#246)")
}

func TestReclaimAgentTasks_DedupesConcurrentForSameAgent(t *testing.T) {
	const id int64 = 4242
	// Pre-seed the in-flight guard to simulate a reclaim already running.
	reclaimInFlightMu.Lock()
	reclaimInFlight[id] = struct{}{}
	reclaimInFlightMu.Unlock()
	t.Cleanup(func() {
		reclaimInFlightMu.Lock()
		delete(reclaimInFlight, id)
		reclaimInFlightMu.Unlock()
	})

	// MockQueue with NO Info expectation: if ReclaimAgentTasks failed to dedupe
	// and called ReleaseAgentTasks, the unexpected Info call would fail the mock.
	q := queue_mocks.NewMockQueue(t)
	rpcPeer := grpcserver.NewRPCForTesting(q, nil)

	ReclaimAgentTasks(id, rpcPeer) // must be skipped by the guard
}

func sysAgent(id, lastContact int64) *model.Agent {
	return &model.Agent{ID: id, OwnerID: model.IDNotSet, LastContact: lastContact}
}

func TestAgentReapableAt(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	staleLC := now.Add(-AgentReapThreshold - time.Minute).Unix()
	cases := []struct {
		name  string
		agent *model.Agent
		want  bool
	}{
		{"nil", nil, false},
		{"never reported (lc=0)", sysAgent(1, 0), false},
		{"fresh system agent", sysAgent(1, now.Add(-time.Minute).Unix()), false},
		{"exactly at threshold (not past)", sysAgent(1, now.Add(-AgentReapThreshold).Unix()), false},
		{"stale system agent", sysAgent(1, staleLC), true},
		// An individually-tokened (owned) agent is never reaped, even when stale —
		// mirrors UnregisterAgent's system-only delete.
		{"individual agent, stale", &model.Agent{ID: 2, OwnerID: 7, LastContact: staleLC}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, agentReapableAt(tc.agent, now))
		})
	}
}

func TestReapOrphanAgents(t *testing.T) {
	orig := AgentReapThreshold
	AgentReapThreshold = 30 * time.Minute
	t.Cleanup(func() { AgentReapThreshold = orig })

	now := time.Now()
	stale := now.Add(-time.Hour).Unix()
	fresh := now.Add(-time.Minute).Unix()
	agents := []*model.Agent{
		sysAgent(1, stale),                      // reapable
		sysAgent(2, fresh),                      // alive — keep
		{ID: 3, OwnerID: 9, LastContact: stale}, // individual — keep
		sysAgent(4, 0),                          // never reported — keep
		sysAgent(5, stale),                      // reapable
	}
	s := store_mocks.NewMockStore(t)
	s.On("AgentList", mock.Anything).Return(agents, nil)
	s.On("AgentDelete", mock.MatchedBy(func(a *model.Agent) bool { return a.ID == 1 || a.ID == 5 })).Return(nil)

	assert.Equal(t, 2, ReapOrphanAgents(s))
	s.AssertNotCalled(t, "AgentDelete", mock.MatchedBy(func(a *model.Agent) bool { return a.ID == 2 || a.ID == 3 || a.ID == 4 }))
}

func TestReapOrphanAgents_Guards(t *testing.T) {
	assert.Equal(t, 0, ReapOrphanAgents(nil))

	t.Run("AgentList error", func(t *testing.T) {
		s := store_mocks.NewMockStore(t)
		s.On("AgentList", mock.Anything).Return(([]*model.Agent)(nil), errors.New("boom"))
		assert.Equal(t, 0, ReapOrphanAgents(s))
	})

	t.Run("delete error is skipped, not counted", func(t *testing.T) {
		orig := AgentReapThreshold
		AgentReapThreshold = 30 * time.Minute
		t.Cleanup(func() { AgentReapThreshold = orig })
		s := store_mocks.NewMockStore(t)
		s.On("AgentList", mock.Anything).Return([]*model.Agent{sysAgent(1, time.Now().Add(-time.Hour).Unix())}, nil)
		s.On("AgentDelete", mock.Anything).Return(errors.New("db down"))
		assert.Equal(t, 0, ReapOrphanAgents(s))
	})
}
