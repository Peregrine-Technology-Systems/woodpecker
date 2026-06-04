// Copyright 2022 Woodpecker Authors
// Copyright 2019 mhmxs.
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
	"time"

	"go.woodpecker-ci.org/woodpecker/v3/pipeline/errors"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/store"
)

// PipelineStatus determine pipeline status based on corresponding workflow list.
//
// Seed with the first workflow's actual state, not a synthetic StatusSuccess.
// The synthetic seed combined with a single killed workflow incorrectly
// yielded StatusPartial after #28 introduced the success+killed → partial
// rule (one killed workflow is not "partial" — partial requires at least one
// actual success AND at least one killed).
//
// Guard: if any workflow is still active (running or pending), the pipeline
// must remain StatusRunning — a canceled cleanup step must not collapse a
// still-executing compile to killed/partial (#111).
func PipelineStatus(workflows []*model.Workflow) model.StatusValue {
	if len(workflows) == 0 {
		return model.StatusSuccess
	}

	for _, w := range workflows {
		if w.State == model.StatusRunning || w.State == model.StatusPending {
			return model.StatusRunning
		}
	}

	status := workflows[0].State
	for _, p := range workflows[1:] {
		status = MergeStatusValues(status, p.State)
	}

	return status
}

func UpdateToStatusRunning(store store.Store, pipeline model.Pipeline, started int64) (*model.Pipeline, error) {
	pipeline.Status = model.StatusRunning
	pipeline.Started = started
	return &pipeline, store.UpdatePipeline(&pipeline)
}

func UpdateToStatusPending(store store.Store, pipeline model.Pipeline, reviewer string) (*model.Pipeline, error) {
	if reviewer != "" {
		pipeline.Reviewer = reviewer
		pipeline.Reviewed = time.Now().Unix()
	}
	pipeline.Status = model.StatusPending
	return &pipeline, store.UpdatePipeline(&pipeline)
}

func UpdateToStatusDeclined(store store.Store, pipeline model.Pipeline, reviewer string) (*model.Pipeline, error) {
	pipeline.Reviewer = reviewer
	pipeline.Status = model.StatusDeclined
	pipeline.Reviewed = time.Now().Unix()
	return &pipeline, store.UpdatePipeline(&pipeline)
}

func UpdateStatusToDone(store store.Store, pipeline model.Pipeline, status model.StatusValue, stopped int64) (*model.Pipeline, error) {
	pipeline.Status = status
	pipeline.Finished = stopped
	// #202: derive KillReason for terminal-kill transitions when not
	// already set by an upstream caller (Cancel() / killOrphan() set it
	// directly via UpdateToStatusKilled or by direct field assignment).
	// The "agent_disconnect" case is detected by the canonical
	// `Workflow.KilledByAgentDisconnect()` signature; everything else
	// falls through to "agent_done_kill" which captures cases where the
	// agent itself reported a Killed/Canceled state.
	if IsTerminalKill(status) && pipeline.KillReason == "" {
		pipeline.KillReason = killReasonFromWorkflows(pipeline.Workflows)
		pipeline.KilledAt = stopped
	}
	return &pipeline, store.UpdatePipeline(&pipeline)
}

// killReasonFromWorkflows derives a KillReason string from the rolled-up
// state of the pipeline's workflows. Inspects each workflow for the
// canonical agent-disconnect signature, then for the agent-shutdown
// (SIGTERM/preemption) signature (#275); falls back to a generic
// "agent_done_kill" tag when neither is observed (the agent itself reported a
// Killed/Canceled state for its own reasons). The agent-shutdown class is
// broken out from the generic fallback so a recoverable spot preemption is
// distinguishable from an arbitrary agent-reported kill in forensics and bus
// telemetry — it is the precondition for the self-heal re-queue follow-up.
func killReasonFromWorkflows(workflows []*model.Workflow) string {
	for _, w := range workflows {
		if w != nil && w.KilledByAgentDisconnect() {
			return "agent_disconnect"
		}
	}
	for _, w := range workflows {
		if w != nil && w.CanceledByAgentShutdown() {
			return "agent_preempted"
		}
	}
	return "agent_done_kill"
}

func UpdateToStatusError(store store.Store, pipeline model.Pipeline, err error) (*model.Pipeline, error) {
	pipeline.Errors = errors.GetPipelineErrors(err)
	pipeline.Status = model.StatusError
	pipeline.Started = time.Now().Unix()
	pipeline.Finished = pipeline.Started
	return &pipeline, store.UpdatePipeline(&pipeline)
}

// IsTerminalKill reports whether a status represents a non-success
// terminal transition that should carry a KillReason attribution (#202).
func IsTerminalKill(s model.StatusValue) bool {
	switch s {
	case model.StatusKilled, model.StatusCanceled, model.StatusSuperseded:
		return true
	default:
		return false
	}
}

// UpdateToStatusKilled sets the pipeline to Killed/Canceled/Superseded.
// killReason is stamped on the pipeline (#202) so ops can attribute the
// transition to a specific code path. Empty killReason is allowed but
// indicates a caller that hasn't been #202-instrumented yet.
func UpdateToStatusKilled(store store.Store, pipeline model.Pipeline, cancelInfo *model.CancelInfo, state model.StatusValue, killReason string) (*model.Pipeline, error) {
	pipeline.Status = state
	now := time.Now().Unix()
	pipeline.Finished = now
	// #263 part 2: stamp the orthogonal cancel trigger from the evidence fields
	// before persisting, so it survives the pending-only masking that KillReason
	// (below) may apply. Derived here, the single storage choke point for every
	// Cancel() path, so callers don't each have to set it.
	if cancelInfo != nil {
		cancelInfo.Trigger = model.CancelTrigger(cancelInfo)
	}
	pipeline.CancelInfo = cancelInfo
	pipeline.KillReason = killReason
	pipeline.KilledAt = now
	return &pipeline, store.UpdatePipeline(&pipeline)
}
