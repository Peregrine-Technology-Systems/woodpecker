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

package constant

import "testing"

// TestGRPCKeepaliveInvariant guards the woodpecker#312 fix: the agent's keepalive
// ping interval must never drop below the server's enforcement floor, or an agent
// ping counts as "too fast" and the server re-issues the ENHANCE_YOUR_CALM /
// too_many_pings GoAway that wedged the idle gRPC agent in the first place. Both
// must also be strictly positive — the original defect was both defaulting to 0.
func TestGRPCKeepaliveInvariant(t *testing.T) {
	if GRPCKeepaliveMinTime <= 0 {
		t.Fatalf("GRPCKeepaliveMinTime must be > 0 (0 was the original too_many_pings defect), got %v", GRPCKeepaliveMinTime)
	}
	if GRPCKeepaliveTime <= 0 {
		t.Fatalf("GRPCKeepaliveTime must be > 0 (0 was the original too_many_pings defect), got %v", GRPCKeepaliveTime)
	}
	if GRPCKeepaliveTime < GRPCKeepaliveMinTime {
		t.Fatalf("GRPCKeepaliveTime (%v) must be >= GRPCKeepaliveMinTime (%v): an agent that pings "+
			"faster than the server's floor re-triggers the too_many_pings GoAway (woodpecker#312)",
			GRPCKeepaliveTime, GRPCKeepaliveMinTime)
	}
}
