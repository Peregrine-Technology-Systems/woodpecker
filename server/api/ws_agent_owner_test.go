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

	grpcserver "go.woodpecker-ci.org/woodpecker/v3/server/rpc"
)

func hasPendingRelease(id int64) bool {
	pendingReleasesMu.Lock()
	defer pendingReleasesMu.Unlock()
	return pendingReleases[id] != nil
}

// #394: a reconnecting agent re-binds its id on a NEW connection, and the old
// one may still be open on the server (a one-sided drop is noticed only at the
// pong deadline). When the stale connection finally closes, it must not
// schedule a release of the id: that would release the live connection's
// in-flight tasks and delete its row 30s later.
func TestWSAgent_StaleConnectionCloseDoesNotReleaseReboundAgent(t *testing.T) {
	const id = int64(61493)
	rpcPeer := &grpcserver.RPC{}
	t.Cleanup(func() { cancelPendingAgentRelease(id); forgetAgentConnection(id) })

	stale := &wsAgentState{agentID: id, rpc: rpcPeer}
	live := &wsAgentState{agentID: id, rpc: rpcPeer}
	claimAgentConnection(id, stale)
	claimAgentConnection(id, live) // re-register on the new connection

	stale.cleanup()
	assert.False(t, hasPendingRelease(id), "the stale connection no longer owns the agent")

	live.cleanup()
	assert.True(t, hasPendingRelease(id), "the owning connection's close still starts the grace window")
}
