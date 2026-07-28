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
	"fmt"
	"slices"

	"github.com/rs/zerolog/log"

	corepipeline "go.woodpecker-ci.org/woodpecker/v3/pipeline"
	"go.woodpecker-ci.org/woodpecker/v3/server"
	"go.woodpecker-ci.org/woodpecker/v3/server/forge"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/plugin"
	"go.woodpecker-ci.org/woodpecker/v3/server/queue"
	"go.woodpecker-ci.org/woodpecker/v3/server/store"
)

// Cancel the pipeline and returns the status.
// When preserveIndependent is true, workflows with no queue dependencies (depends_on: [])
// are left running — used by cancelPreviousPipelines to avoid killing deploy workflows
// on healthy agents when a new CI push supersedes a previous one (#822).
func Cancel(ctx context.Context, _forge forge.Forge, _store store.Store, repo *model.Repo, user *model.User, pipeline *model.Pipeline, cancelInfo *model.CancelInfo, preserveIndependent bool) error {
	if pipeline.Status != model.StatusRunning && pipeline.Status != model.StatusPending && pipeline.Status != model.StatusBlocked {
		return &ErrBadRequest{Msg: "Cannot cancel a non-running or non-pending or non-blocked pipeline"}
	}

	workflows, err := _store.WorkflowGetTree(pipeline)
	if err != nil {
		return &ErrNotFound{Msg: err.Error()}
	}

	// Build set of independent workflow IDs (no queue dependencies).
	// Uses the persisted DependsOn field on workflows, NOT transient queue tasks.
	independentIDs := make(map[int64]bool)
	if preserveIndependent {
		independentIDs = findIndependentWorkflows(workflows)
	}

	// First cancel/evict workflows in the queue in one go
	var workflowsToCancel []string
	for _, w := range workflows {
		if w.State == model.StatusRunning || w.State == model.StatusPending {
			if independentIDs[w.ID] {
				log.Info().Msgf("cancel: preserving independent workflow %d (%s) (#822)", w.ID, w.Name)
				continue
			}
			workflowsToCancel = append(workflowsToCancel, fmt.Sprint(w.ID))
		}
	}

	if len(workflowsToCancel) != 0 {
		if err := server.Config.Services.Queue.ErrorAtOnce(ctx, workflowsToCancel, queue.ErrCancel); err != nil {
			log.Error().Err(err).Msgf("queue: evict_at_once: %v", workflowsToCancel)
		}
	}

	// #891: Also call Done() for running workflows. ErrorAtOnce removes tasks
	// from pending/waiting, but running tasks assigned to dead agents stay in
	// the queue's running map until the agent calls rpc.Done() — which never
	// happens if the agent crashed. Done() ensures cleanup regardless.
	for _, w := range workflows {
		if w.State == model.StatusRunning && !independentIDs[w.ID] {
			if err := server.Config.Services.Queue.Done(ctx, fmt.Sprint(w.ID), model.StatusKilled); err != nil {
				log.Debug().Err(err).Msgf("queue.Done for running workflow %d (may already be removed)", w.ID)
			}
		}
	}

	// Check if any workflows are still running (preserved independent ones)
	hasPreservedRunning := false
	hasPendingOnly := true

	// Then update the DB status for pending pipelines
	// Running ones will be set when the agents stop on the cancel signal
	for _, workflow := range workflows {
		if independentIDs[workflow.ID] {
			if workflow.State == model.StatusRunning || workflow.State == model.StatusPending {
				hasPreservedRunning = true
			}
			continue
		}
		if workflow.State == model.StatusPending {
			if _, err = UpdateWorkflowToStatusSkipped(_store, *workflow); err != nil {
				log.Error().Err(err).Msgf("cannot update workflow with id %d state", workflow.ID)
			}
		} else {
			hasPendingOnly = false
		}
		for _, step := range workflow.Children {
			if step.State == model.StatusPending {
				if _, err = UpdateStepToStatusSkipped(_store, *step, 0, model.StatusCanceled); err != nil {
					log.Error().Err(err).Msgf("cannot update workflow with id %d state", workflow.ID)
				}
			}
		}
	}

	// If independent workflows are still running, don't kill the pipeline — let them finish.
	// Pipeline status will be computed when all workflows complete (rpc.Done).
	if hasPreservedRunning {
		log.Info().Msgf("cancel: pipeline %d has preserved independent workflows — staying running (#822)", pipeline.ID)
		return nil
	}

	// Use superseded status when canceled by a newer push, killed otherwise (woodpecker-server#7)
	plState := model.StatusKilled
	eventType := plugin.EventPipelineKilled
	reason := "user_initiated"
	if cancelInfo != nil && cancelInfo.SupersededBy > 0 {
		plState = model.StatusSuperseded
		eventType = plugin.EventPipelineSuperseded
		reason = "superseded_by_newer_push"
	}
	if hasPendingOnly {
		plState = model.StatusCanceled
		reason = "pending_only_canceled"
	}
	// #193: every pipeline kill/cancel/supersede gets a structured INFO log
	// with explicit reason + never_dispatched flag. Lets ops grep for mode-B
	// "killed before dispatch" incidents without sampling pipelines table.
	log.Info().
		Int64("pipeline_id", pipeline.ID).
		Str("repo", repo.FullName).
		Str("prior_status", string(pipeline.Status)).
		Str("new_status", string(plState)).
		Bool("never_dispatched", pipeline.Started == 0).
		Str("reason", reason).
		Msg("pipeline cancel: transitioning")
	killedPipeline, err := UpdateToStatusKilled(_store, *pipeline, cancelInfo, plState, reason)
	if err != nil {
		log.Error().Err(err).Msgf("UpdateToStatusKilled: %v", pipeline)
		return err
	}

	EmitEvent(eventType, repo, killedPipeline, "")
	updatePipelineStatus(ctx, _forge, killedPipeline, repo, user)

	if killedPipeline.Workflows, err = _store.WorkflowGetTree(killedPipeline); err != nil {
		return err
	}
	publishToTopic(killedPipeline, repo)

	return nil
}

// findIndependentWorkflows returns workflow IDs that have no dependencies.
// These are workflows with depends_on: [] in the pipeline config — they should
// not be canceled when a pipeline is superseded by a new push (#822).
//
// Uses the persisted DependsOn field on the Workflow model, NOT transient queue
// tasks. The previous implementation used store.TaskList() which only returns
// tasks still in the queue — once an agent picks up a task (Poll), it's deleted
// from the task store. This caused running independent workflows to be killed
// because they were no longer visible in TaskList().
func findIndependentWorkflows(workflows []*model.Workflow) map[int64]bool {
	result := make(map[int64]bool)
	for _, w := range workflows {
		if len(w.DependsOn) == 0 {
			result[w.ID] = true
		}
	}
	return result
}

// hasInflightDeployWorkflow reports whether any not-yet-finished (running or
// pending) workflow is deploy-class (#294). Such a pipeline must not be
// cancelled by a successor push: deploy/promote/version-bump work isn't
// re-runnable, so it can be neither preemptible (#266) nor cancellable-by-
// successor (this). Uses the exact #266 deploy-pattern signal
// (corepipeline.ShouldForceOndemand) so the two stay in lockstep — a workflow
// the server forces to ondemand is also one it refuses to reap.
func hasInflightDeployWorkflow(workflows []*model.Workflow, isTagEvent bool) bool {
	patterns := corepipeline.DeployPatterns()
	for _, w := range workflows {
		if w.State != model.StatusRunning && w.State != model.StatusPending {
			continue
		}
		if corepipeline.ShouldForceOndemand(w.Name, isTagEvent, patterns) {
			return true
		}
	}
	return false
}

func cancelPreviousPipelines(
	ctx context.Context,
	_forge forge.Forge,
	_store store.Store,
	pipeline *model.Pipeline,
	repo *model.Repo,
	user *model.User,
) error {
	// check this event should cancel previous pipelines
	eventIncluded := slices.Contains(repo.CancelPreviousPipelineEvents, pipeline.Event)
	if !eventIncluded {
		return nil
	}

	// get all active activeBuilds
	activeBuilds, err := _store.GetActivePipelineList(repo)
	if err != nil {
		return err
	}

	pipelineNeedsCancel := func(active *model.Pipeline) bool {
		// always filter on same event
		if active.Event != pipeline.Event {
			return false
		}

		// #338: a duplicate/redelivered webhook for the same commit must never
		// cancel its own twin. Without this, a GitHub redelivery (or a genuine
		// double-delivery) of an already-processed push creates a second
		// pipeline for the identical commit, which this function would
		// otherwise treat as "a newer push superseding an older one on this
		// branch" and cancel the original — throwing away in-flight progress
		// and restarting CI from zero for no reason at all, since both
		// pipelines are building the exact same code. Guarded on non-empty:
		// an unset Commit ("") is "unknown," not "matches" — two unrelated
		// pipelines that both happen to have no recorded commit must not be
		// treated as duplicates of each other.
		if pipeline.Commit != "" && pipeline.Commit == active.Commit {
			return false
		}

		// find events for the same context
		switch pipeline.Event {
		case model.EventPush:
			return pipeline.Branch == active.Branch
		default:
			return pipeline.Refspec == active.Refspec
		}
	}

	for _, active := range activeBuilds {
		if active.ID == pipeline.ID {
			// same pipeline. e.g. self
			continue
		}

		cancel := pipelineNeedsCancel(active)

		if !cancel {
			continue
		}

		// [pts] #294 — never let a successor push reap an in-flight deploy-class
		// pipeline. Deploy/promote/version-bump work is not safely re-runnable
		// (the killed-workflow re-queue, #224/#225/#235, deliberately won't
		// restart a workflow that did observable work), so — mirroring #266's
		// ondemand force — a pipeline with a pending/running deploy-class
		// workflow is preserved, not cancelled. Same deploy-pattern signal as
		// #266 (corepipeline.ShouldForceOndemand), kept in lockstep by reuse.
		// This is broader than the #822 independent-workflow preservation: it
		// spares the WHOLE pipeline (deploy workflows commonly DO have deps).
		activeWorkflows, werr := _store.WorkflowGetTree(active)
		if werr != nil {
			// Fail-safe: if we can't inspect the pipeline, do NOT cancel it. A
			// wrongly-reaped deploy is silent and unrecoverable; a wrongly-spared
			// CI run merely finishes on its own. Preserve on uncertainty.
			log.Warn().Err(werr).Int64("id", active.ID).
				Msg("cancel-previous: cannot load workflows, preserving pipeline (#294)")
			continue
		}
		if hasInflightDeployWorkflow(activeWorkflows, active.Event == model.EventTag) {
			log.Info().Int64("id", active.ID).Int64("superseded_by", pipeline.Number).
				Msg("cancel-previous: preserving pipeline with in-flight deploy-class workflow (#294)")
			continue
		}

		if err = Cancel(ctx, _forge, _store, repo, user, active, &model.CancelInfo{
			SupersededBy: pipeline.Number,
		}, true); err != nil {
			log.Error().
				Err(err).
				Str("ref", active.Ref).
				Int64("id", active.ID).
				Msg("failed to cancel pipeline")
		}
	}

	return nil
}
