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
	"time"

	"go.woodpecker-ci.org/woodpecker/v3/server/store"
	"go.woodpecker-ci.org/woodpecker/v3/server/store/types"
)

// IsAgentLiveForReconcile is the orphan reconciler's hold oracle (#394): may
// this agent still be running the workflow it holds? It reads LastContact from
// the store, which both transports refresh on every health beat, so it covers
// gRPC agents and survives a server restart.
//
// It gates an irreversible kill, so it fails toward "live". Only positive
// evidence that the agent is gone returns false: its row is deleted, it never
// reported, or its last beat is older than AgentStaleThreshold.
func IsAgentLiveForReconcile(agentID int64, s store.Store) bool {
	if agentID <= 0 {
		return false
	}
	if s == nil {
		return true
	}
	agent, err := s.AgentFind(agentID)
	if errors.Is(err, types.ErrRecordNotExist) {
		return false
	}
	if err != nil || agent == nil {
		return true
	}
	if agent.LastContact <= 0 {
		return false
	}
	return time.Since(time.Unix(agent.LastContact, 0)) <= AgentStaleThreshold
}
