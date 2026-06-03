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
//
// Level gating (#276): a flat INFO line per request made woodpecker-server
// ~96.6% of the ci-runners-de fleet's Cloud Logging ingestion (~44 GiB/mo) —
// ~95% of which is successful high-frequency GET polling of pipeline/queue
// state (the UI, scaler, and agents). Emitting that flood at INFO buys no
// signal. We keep the #215 value by selecting the level from the outcome
// instead of hardwiring INFO (see levelFor): errors and mutating requests
// stay visible at the default log level, the GET poll flood drops to DEBUG.
package accesslog

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// skipPaths are excluded from access logging — high-frequency, low-signal
// endpoints that would drown out genuine access events in journalctl.
var skipPaths = map[string]bool{
	"/healthz": true,
	"/metrics": true,
}

// levelFor selects the zerolog level for a completed request (#276). The goal
// is to suppress the high-frequency successful-GET poll flood at the default
// INFO level while never inferring a result by the absence of a line for any
// request that carries audit or failure signal:
//
//   - status >= 400        → Warn  (every error stays visible — preserves #215)
//   - mutating method       → Info  (POST/PUT/PATCH/DELETE: webhooks, priority
//     changes — low volume, high audit value; absence
//     must never be read as "filtered, not failed")
//   - successful read (GET/HEAD/…) → Debug (the ~95% poll flood — gated off by
//     default; flip WOODPECKER_LOG_LEVEL=debug to see it)
func levelFor(method string, status int) zerolog.Level {
	if status >= http.StatusBadRequest {
		return zerolog.WarnLevel
	}
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return zerolog.InfoLevel
	default:
		return zerolog.DebugLevel
	}
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

		status := c.Writer.Status()
		log.WithLevel(levelFor(c.Request.Method, status)).
			Str("method", c.Request.Method).
			Str("path", path).
			Int("status", status).
			Int64("latency_ms", time.Since(start).Milliseconds()).
			Str("remote_ip", c.ClientIP()).
			Str("user_agent", c.Request.UserAgent()).
			Msg("http")
	}
}
