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
	"google.golang.org/api/option"
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
// logDrainWriteErrorType is a stable field value for filtering/alerting on
// this exact event, independent of the human-readable Msg() text — the same
// reason this repo's testing discipline requires typed errors at substrate
// boundaries (errors.Is, never string-matching the message). If the wording
// in Msg() ever changes, anything matching on this field keeps working.
const logDrainWriteErrorType = "log_drain_write_error"

func newOnErrorHandler() func(error) {
	var failureCount atomic.Uint64
	return func(e error) {
		n := failureCount.Add(1)
		log.Error().Err(e).Str("type", logDrainWriteErrorType).Uint64("consecutive_failures", n).
			Msg("log drain: Cloud Logging write error (best-effort, dropped — but NOT silent)")
	}
}

// New builds a Drain backed by a real GCP Cloud Logging client. By default it
// authenticates via the process's global Application Default Credentials. It
// NEVER returns an error or crashes the server: an empty project (drain not
// configured) or unavailable/invalid credentials (e.g. local dev, or a bad
// credentialsFile) yields a disabled no-op Drain after a one-time INFO log.
// Async transport errors are logged at ERROR via the client's error handler —
// loud on purpose, never silent-OK (#333). (#233)
//
// credentialsFile (#343): when non-empty, a path to a GCP credentials file
// (service-account key or a WIF external_account config) the drain
// authenticates with INSTEAD of global ADC — scoped to this one client, so it
// never repoints any other ADC consumer in the process (the footgun
// GOOGLE_APPLICATION_CREDENTIALS would cause if used process-wide just to
// give the drain a distinct identity). Empty preserves the original
// global-ADC behavior exactly.
func New(ctx context.Context, project, logName, credentialsFile string) *Drain {
	if project == "" {
		log.Info().Msg("log drain: WOODPECKER_LOG_DRAIN_GCP_PROJECT unset — step-log Cloud Logging drain disabled")
		return &Drain{}
	}
	if logName == "" {
		logName = DefaultLogName
	}

	var clientOpts []option.ClientOption
	if credentialsFile != "" {
		clientOpts = append(clientOpts, option.WithCredentialsFile(credentialsFile))
	}

	client, err := logging.NewClient(ctx, project, clientOpts...)
	if err != nil {
		// Most commonly: ADC unavailable, or an invalid/unreadable
		// credentialsFile. Disable gracefully either way, don't crash.
		log.Info().Err(err).Str("project", project).Bool("credentials_file_set", credentialsFile != "").
			Msg("log drain: Cloud Logging client unavailable — step-log drain disabled")
		return &Drain{}
	}
	client.OnError = newOnErrorHandler()

	logger := client.Logger(logName)
	fullLogName := fmt.Sprintf("projects/%s/logs/%s", project, logName)
	log.Info().Str("project", project).Str("log", fullLogName).Msg("log drain: step-log Cloud Logging drain enabled")
	return newDrain(logger, client.Close)
}
