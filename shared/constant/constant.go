// Copyright 2022 Woodpecker Authors
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

import "time"

// DefaultConfigOrder represent the priority in witch woodpecker search for a pipeline config by default
// folders are indicated by supplying a trailing slash.
var DefaultConfigOrder = [...]string{
	".woodpecker/",
	".woodpecker.yaml",
	".woodpecker.yml",
}

const (
	// DefaultClonePlugin can be changed by 'WOODPECKER_DEFAULT_CLONE_PLUGIN' at runtime.
	// renovate: datasource=docker depName=woodpeckerci/plugin-git
	DefaultClonePlugin = "docker.io/woodpeckerci/plugin-git:2.8.1"
)

// TrustedClonePlugins can be changed by 'WOODPECKER_PLUGINS_TRUSTED_CLONE' at runtime.
var TrustedClonePlugins = []string{
	DefaultClonePlugin,
	"docker.io/woodpeckerci/plugin-git",
	"quay.io/woodpeckerci/plugin-git",
}

// TaskTimeout is the queue lease duration — how long before an unextended task is requeued.
// The WebSocket heartbeat hub (20s orphan detection) is the primary mechanism for detecting
// dead agents. TaskTimeout is a safety net only — set high to avoid false expiry under CPU load.
// Must be >= WOODPECKER_TIMEOUT (15m) — deploy workflows take 5-10min and the agent's gRPC
// Extend calls may fail silently through Caddy. 5min was too short (#3360 killed mid-deploy).
// History: 60s (original) → 15s (too aggressive) → 5min (#162) → 15min (#3360).
var TaskTimeout = 15 * time.Minute

// gRPC keepalive coordination (woodpecker#312).
//
// Both the agent's keepalive-time and the server's keepalive-min-time previously
// defaulted to 0, and the server's gRPC EnforcementPolicy did not permit keepalive
// pings without an active stream. So an *idle* gRPC agent (the d3ci42-local box
// mostly sits idle, running only occasional host-pinned work) would send keepalive
// pings the server treated as abusive, the server GoAway'd the connection with
// ENHANCE_YOUR_CALM / "too_many_pings", and the agent's workflow-RPC channel
// (Next/wait/update) wedged on DeadlineExceeded — while its heartbeat path stayed
// alive, so it kept *claiming* workflows it could no longer *report*, and steps
// came back skipped (pts-build bakes wedged). Confirmed live 2026-07-11. See the
// grpc-go too_many_pings guidance (grpc-go #717).
//
// The fix couples the two sides (both set PermitWithoutStream=true, in
// cmd/agent + cmd/server) and pins these as the flag defaults. The load-bearing
// invariant: the agent's ping interval MUST be >= the server's floor, or an agent
// ping is "too fast" and the GoAway returns. Enforced by TestGRPCKeepaliveInvariant.
const (
	// GRPCKeepaliveTime is the agent default for WOODPECKER_KEEPALIVE_TIME: how long
	// the agent waits with no activity before pinging the server to check the
	// transport is alive. Must be >= GRPCKeepaliveMinTime.
	GRPCKeepaliveTime = 30 * time.Second

	// GRPCKeepaliveMinTime is the server default for WOODPECKER_KEEPALIVE_MIN_TIME:
	// the minimum interval a client must wait between keepalive pings before the
	// server's enforcement treats them as abusive. Must be <= GRPCKeepaliveTime.
	GRPCKeepaliveMinTime = 10 * time.Second
)
