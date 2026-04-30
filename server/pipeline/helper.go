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

package pipeline

import (
	"context"
	"time"

	"github.com/rs/zerolog/log"

	"go.woodpecker-ci.org/woodpecker/v3/server/forge"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
)

// forgeStatusTimeout caps each forge.Status call. A slow / hanging forge
// (#30) must not block the pipeline state machine. Worst case per pipeline
// is forgeStatusTimeout × len(Workflows).
const forgeStatusTimeout = 5 * time.Second

func updatePipelineStatus(ctx context.Context, forge forge.Forge, pipeline *model.Pipeline, repo *model.Repo, user *model.User) {
	for _, workflow := range pipeline.Workflows {
		// fork#44: don't post errored to GitHub on agent-disconnect kills.
		// See updateForgeStatus in server/rpc/rpc.go for the full rationale.
		if workflow.KilledByAgentDisconnect() {
			log.Info().
				Str("repo", repo.FullName).
				Int64("pipeline_id", pipeline.ID).
				Int64("workflow_id", workflow.ID).
				Str("error", workflow.Error).
				Msg("forge.Status: skipping post for agent-disconnect kill (#44)")
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, forgeStatusTimeout)
		err := forge.Status(callCtx, user, repo, pipeline, workflow)
		cancel()
		if err != nil {
			log.Error().Err(err).Msgf("error setting commit status for %s/%d", repo.FullName, pipeline.Number)
			return
		}
	}
}
