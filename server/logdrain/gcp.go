// Copyright 2026 Peregrine Technology Systems
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

package logdrain

import (
	"context"
	"fmt"
	"sync/atomic"

	"cloud.google.com/go/logging"
	"github.com/rs/zerolog/log"
)

// DefaultLogName is used when WOODPECKER_LOG_DRAIN_GCP_LOG_NAME is unset.
const DefaultLogName = "woodpecker-steps"

// newOnErrorHandler builds the callback passed to the GCP Cloud Logging
// client's async OnError hook. Extracted from New() so the escalation logic
// is unit-testable without a live GCP client (#333).
//
// The drain is best-effort by design (must never affect the SQLite write or
// the agent's gRPC ack), but a best-effort failure must still be LOUD, not
// silent-OK — the prior WARN-only handler let every single write fail
// unnoticed for as long as the drain was deployed. Escalate to ERROR
// (severity-based alerting/log-based monitoring catches it) and carry a
// running failure count so "one transient blip" and "systemically broken
// since startup" are distinguishable from the log line alone.
func newOnErrorHandler() func(error) {
	var failureCount atomic.Uint64
	return func(e error) {
		n := failureCount.Add(1)
		log.Error().Err(e).Uint64("consecutive_failures", n).
			Msg("log drain: Cloud Logging write error (best-effort, dropped — but NOT silent)")
	}
}

// New builds a Drain backed by a real GCP Cloud Logging client using
// Application Default Credentials. It NEVER returns an error or crashes the
// server: an empty project (drain not configured) or unavailable ADC (e.g.
// local dev) yields a disabled no-op Drain after a one-time INFO log. Async
// transport errors are logged at ERROR via the client's error handler — loud
// on purpose, never silent-OK (#333). (#233)
func New(ctx context.Context, project, logName string) *Drain {
	if project == "" {
		log.Info().Msg("log drain: WOODPECKER_LOG_DRAIN_GCP_PROJECT unset — step-log Cloud Logging drain disabled")
		return &Drain{}
	}
	if logName == "" {
		logName = DefaultLogName
	}

	client, err := logging.NewClient(ctx, project)
	if err != nil {
		// Most commonly: ADC not available. Disable gracefully, don't crash.
		log.Info().Err(err).Str("project", project).
			Msg("log drain: Cloud Logging client unavailable (ADC?) — step-log drain disabled")
		return &Drain{}
	}
	client.OnError = newOnErrorHandler()

	logger := client.Logger(logName)
	fullLogName := fmt.Sprintf("projects/%s/logs/%s", project, logName)
	log.Info().Str("project", project).Str("log", fullLogName).Msg("log drain: step-log Cloud Logging drain enabled")
	return newDrain(logger, client.Close)
}
