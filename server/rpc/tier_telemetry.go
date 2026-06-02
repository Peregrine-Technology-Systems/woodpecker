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
	"github.com/rs/zerolog/log"

	corepipeline "go.woodpecker-ci.org/woodpecker/v3/pipeline"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/pipeline"
	"go.woodpecker-ci.org/woodpecker/v3/server/plugin"
)

// tierSpot is the only agent tier that is preemptible (GCP spot). The persistent
// local box (backend: local-d3ci42) and the on-demand/n2 GCP classes are not
// spot and legitimately run deploy-class work, so the #268 bypass signal keys on
// this exact value — an untiered agent is NOT treated as spot (that would
// false-positive on every local deploy).
const tierSpot = "spot"

// shouldEmitTierBypass is the pure #268 decision: a spot-tier agent is taking a
// deploy-class task. Deploy-class means the workflow name matches a deploy
// pattern (same matcher as the dispatch filter and the #266 submit-time rewrite)
// OR the pipeline event is a tag/deployment. With #266 working this is never
// true on a spot agent — a deploy-class task carries tier=ondemand and never
// matches a spot agent — so a true result means the rewrite was bypassed.
func shouldEmitTierBypass(agentTier, workflowName string, event model.WebhookEvent, patterns []string) bool {
	if agentTier != tierSpot {
		return false
	}
	if isDeployWorkflow(workflowName, patterns) {
		return true
	}
	return event == model.EventTag || event == model.EventDeploy
}

// emitTierBypassIfSpotDeploy emits an observe-only ci-events telemetry signal
// (plugin.EventTierBypass) when a spot-tier agent is about to claim a deploy-class
// task — defense-in-depth behind the #266 submit-time tier rewrite (issue #268).
//
// It deliberately does NOT refuse the task: the agent has no decline/re-dispatch
// RPC, so refusing would mean failing the release pipeline (via Done-with-error),
// which is strictly worse than running it once on a spot VM. Instead it makes the
// bypass observable on the bus so monitoring can alert before the next spot
// preemption strands a release. Best-effort throughout: a missing pipeline/repo
// is logged at debug and skipped — telemetry never blocks or fails dispatch.
//
// The cheap tier pre-check short-circuits before any store read, so the common
// case (ondemand/n2/local agents, or a spot agent pulling ordinary CI) costs
// nothing. Emitted once per task pickup (Poll hands a task out exactly once), so
// there is no repeat-storm — unlike the scoring filter, which runs many times.
func (s *RPC) emitTierBypassIfSpotDeploy(agent *model.Agent, task *model.Task) {
	if agent == nil || task == nil || agent.CustomLabels == nil {
		return
	}
	if agent.CustomLabels[corepipeline.LabelFilterTier] != tierSpot {
		return
	}

	pl, err := s.store.GetPipeline(task.PipelineID)
	if err != nil || pl == nil {
		log.Debug().Err(err).Int64("pipeline_id", task.PipelineID).
			Msg("[pts] #268: could not load pipeline for tier-bypass check — skipping telemetry")
		return
	}

	if !shouldEmitTierBypass(tierSpot, task.Name, pl.Event, s.deployPatterns) {
		return
	}

	repo, err := s.store.GetRepo(pl.RepoID)
	if err != nil || repo == nil {
		log.Debug().Err(err).Int64("repo_id", pl.RepoID).
			Msg("[pts] #268: could not load repo for tier-bypass telemetry — skipping")
		return
	}

	log.Warn().
		Str("repo", repo.FullName).
		Int64("pipeline", pl.Number).
		Str("workflow", task.Name).
		Str("agent", agent.Name).
		Int64("agent_id", agent.ID).
		Str("event", string(pl.Event)).
		Msg("[pts] #268: spot agent claiming a deploy-class task — #266 tier rewrite was bypassed; emitting tier_bypass bus telemetry")

	pipeline.EmitEvent(plugin.EventTierBypass, repo, pl, task.Name)
}
