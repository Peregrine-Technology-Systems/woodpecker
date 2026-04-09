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
