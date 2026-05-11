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

// Package accesslog provides a Gin middleware that logs each completed HTTP
// request as a structured zerolog INFO line. This gives operators per-request
// visibility (method, path, status, latency) from journalctl alongside all
// other server log events, without needing to grep Caddy's warn-only logs.
//
// Motivation (#215): the 2026-05-11 outage post-mortem could not split
// woodpecker_webhooks_dropped{reason="pipeline_create_failed"} into
// ErrFiltered (HTTP 204, normal) vs real failures (HTTP 500) because the
// server had no HTTP access log. A single journalctl grep would have resolved
// the question in seconds.
package accesslog

import (
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog/log"
)

// skipPaths are excluded from access logging — high-frequency, low-signal
// endpoints that would drown out genuine access events in journalctl.
var skipPaths = map[string]bool{
	"/healthz": true,
	"/metrics": true,
}

// Middleware returns a Gin HandlerFunc that logs each completed request.
// WebSocket upgrades (/ws/agent) are logged once at connection close with
// the final status code (101 on upgrade, or the error code on rejection).
// The Authorization header and request body are never logged.
func Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		if skipPaths[path] {
			c.Next()
			return
		}

		start := time.Now()
		c.Next()

		log.Info().
			Str("method", c.Request.Method).
			Str("path", path).
			Int("status", c.Writer.Status()).
			Int64("latency_ms", time.Since(start).Milliseconds()).
			Str("remote_ip", c.ClientIP()).
			Str("user_agent", c.Request.UserAgent()).
			Msg("http")
	}
}
