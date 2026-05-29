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
	t.Cleanup(func() { markAgentDisconnected(id) })

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

	ReclaimAgentTasks(99, rpcPeer)

	reclaimInFlightMu.Lock()
	_, busy := reclaimInFlight[99]
	reclaimInFlightMu.Unlock()
	assert.False(t, busy, "guard must be released after reclaim returns")
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
