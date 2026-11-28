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

package agent

import (
	"context"
	"time"

	"github.com/rs/zerolog"

	"go.woodpecker-ci.org/woodpecker/v3/shared/constant"
)

// reconnectNotifier is a transport that can tell the connection came back
// (the gRPC client, #432). WS renews through its health report instead.
type reconnectNotifier interface {
	Reconnected() <-chan struct{}
}

// reconnected returns the transport's next-reconnect channel, or nil (never
// ready) when it cannot tell.
func (r *Runner) reconnected() <-chan struct{} {
	if n, ok := r.client.(reconnectNotifier); ok {
		return n.Reconnected()
	}
	return nil
}

// renewLease extends the workflow's lease every TaskTimeout/3, and at once
// whenever the connection comes back (#432), until ctx ends.
func (r *Runner) renewLease(ctx context.Context, workflowID string, logger zerolog.Logger) {
	reconnected := r.reconnected()
	for {
		select {
		case <-ctx.Done():
			logger.Debug().Msg("workflow context done")
			return

		case <-reconnected:
			reconnected = r.reconnected()
			logger.Info().Msg("connection re-established, renewing workflow lease (#432)")
			r.extend(ctx, workflowID, logger)

		case <-time.After(constant.TaskTimeout / 3):
			logger.Debug().Msg("renewing workflow lease")
			r.extend(ctx, workflowID, logger)
		}
	}
}

func (r *Runner) extend(ctx context.Context, workflowID string, logger zerolog.Logger) {
	if err := r.client.Extend(ctx, workflowID); err != nil {
		logger.Error().Err(err).Msg("failed to extend workflow lease")
	}
}
