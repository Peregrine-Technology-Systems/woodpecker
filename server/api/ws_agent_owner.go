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

import "sync"

// #394: which WS connection currently owns each agent id. Since a reconnecting
// agent re-binds its id on a new connection, the old connection can still be
// open on the server (a one-sided drop is only noticed at the pong deadline).
// Only the owning connection's close may start the #208 grace window;
// otherwise a stale socket closing late would release the live connection's
// in-flight tasks and delete its row.
var (
	agentOwnersMu sync.Mutex
	agentOwners   = make(map[int64]*wsAgentState)
)

// claimAgentConnection makes conn the owner of agentID. Called on register.
func claimAgentConnection(agentID int64, conn *wsAgentState) {
	agentOwnersMu.Lock()
	agentOwners[agentID] = conn
	agentOwnersMu.Unlock()
}

// releaseAgentConnection reports whether conn owned agentID, and if so drops
// the ownership. A false return means a newer connection has re-bound the id.
func releaseAgentConnection(agentID int64, conn *wsAgentState) bool {
	agentOwnersMu.Lock()
	defer agentOwnersMu.Unlock()
	if agentOwners[agentID] != conn {
		return false
	}
	delete(agentOwners, agentID)
	return true
}

// forgetAgentConnection drops any ownership of agentID (tests).
func forgetAgentConnection(agentID int64) {
	agentOwnersMu.Lock()
	delete(agentOwners, agentID)
	agentOwnersMu.Unlock()
}
