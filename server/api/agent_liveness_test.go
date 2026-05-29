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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"go.woodpecker-ci.org/woodpecker/v3/server/queue"
	queue_mocks "go.woodpecker-ci.org/woodpecker/v3/server/queue/mocks"
	grpcserver "go.woodpecker-ci.org/woodpecker/v3/server/rpc"
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
