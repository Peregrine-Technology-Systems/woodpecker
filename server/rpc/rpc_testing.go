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

package grpc

import (
	"go.woodpecker-ci.org/woodpecker/v3/server/queue"
	"go.woodpecker-ci.org/woodpecker/v3/server/store"
)

// NewRPCForTesting constructs an RPC with the given queue + store but
// WITHOUT the side effect of promauto-registering Prometheus collectors
// against the global registry. NewRPC itself panics on the second call
// in the same process because the same metric names are already
// registered; tests that want a fresh RPC per case go through this
// constructor instead.
//
// Caller's caveat: code paths that increment pipelineTime / pipelineCount /
// rpcUpdateRejectedTotal will nil-deref because those fields are NOT set.
// In practice the WS-handler tests in `server/api/ws_agent_test.go` don't
// reach those paths (they use Init/Done/Update/Log/Extend/ReportHealth
// only). Add explicit asserts in any test that strays.
//
// This file is build-tagged-test-only by name (`*_testing.go`) per the
// upstream Woodpecker convention; it ships in the binary but is harmless
// because the constructor is only called from tests.
func NewRPCForTesting(q queue.Queue, s store.Store) *RPC {
	return &RPC{
		queue: q,
		store: s,
	}
}
