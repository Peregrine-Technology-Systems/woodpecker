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

package pipeline

import (
	"context"

	"github.com/rs/zerolog/log"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/store"
)

// FinalizeKilledWorkflow brings one workflow — and every step underneath it
// — to a terminal state. It is the single routine both agent-disconnect
// paths must call: server/rpc.ReleaseAgentTasks (an explicit WS/heartbeat
// disconnect, per-workflow) and OrphanReconciler.killOrphan (the periodic
// DB-sweep backstop, whole-pipeline). Before this extraction the two paths
// independently reimplemented this logic, and killOrphan's copy was a
// partial subset — it flipped the workflow's own State but never touched
// Step rows, and never finalized a workflow that had not even started yet.
// That gap is how a killed pipeline could leave running steps stuck at
// "running" forever and its GitHub status stuck at "pending" forever
// (woodpecker#349). Routing both paths through one function means the next
// thing added to finalization — another step field, another status
// derivation — is automatically in both, instead of drifting the way the
// estate's detect_project_type helper drifted across three hand-synced
// copies (global-claude#44/#57/#259).
//
// errSignature is recorded on every step that gets killed, and on the
// workflow's Error field too if any step did. It also decides whether
// Workflow.KilledByAgentDisconnect() — and so forge-status suppression,
// fork#44 — applies: that check keys on the literal substring "agent
// disconnected". The primary path passes exactly that: a requeue, or the
// ci-scaler auto-requeue-by-repush (#741), is expected to follow up with a
// fresh status, so leaving the previous status in place is correct there.
// killOrphan has no such follow-up on this path (#349's second finding), so
// it MUST pass a different signature — suppressing the post here would
// leave the GitHub status stuck with nothing left to ever resolve it.
func FinalizeKilledWorkflow(ctx context.Context, _store store.Store, workflow *model.Workflow, now int64, errSignature string) {
	if workflow.Children == nil {
		var err error
		workflow.Children, err = _store.StepListFromWorkflowFind(workflow)
		if err != nil {
			log.Error().Err(err).Int64("workflow_id", workflow.ID).
				Msg("finalize: failed to load workflow steps")
		}
	}

	anyKilled := false
	for _, step := range workflow.Children {
		if step.Running() {
			step.State = model.StatusKilled
			step.Finished = now
			step.Error = errSignature
			if err := _store.StepUpdate(step); err != nil {
				log.Error().Err(err).Int64("step_id", step.ID).
					Msg("finalize: failed to kill step")
			}
			anyKilled = true
		}
	}

	// #235: give a killed step that declares an outcome-verification
	// proof-query one read-only probe — covers "killed mid-deploy but the
	// deploy actually landed" (peregrine-ci-scaler#1055). Recomputes
	// anyKilled from the reconciled result so the workflow status below
	// reflects the real outcome.
	if n := ReconcileVerifiedKilledSteps(ctx, _store, workflow); n > 0 {
		log.Info().Int64("workflow_id", workflow.ID).Int("reconciled_steps", n).
			Msg("verify: reconciled disconnect-killed step(s) to success (#235)")
		anyKilled = false
		for _, st := range workflow.Children {
			if st.State == model.StatusKilled {
				anyKilled = true
				break
			}
		}
	}

	workflow.Finished = now
	switch {
	case anyKilled:
		workflow.State = model.StatusKilled
		workflow.Error = errSignature
	case len(workflow.Children) == 0:
		// No step rows at all — e.g. a workflow reconciled before it was
		// ever scheduled. Nothing to derive a real outcome from; a bare
		// kill, not WorkflowStatus's all-success default over an empty
		// slice.
		workflow.State = model.StatusKilled
		workflow.Error = errSignature
	default:
		workflow.State = WorkflowStatus(workflow.Children)
	}
	if err := _store.WorkflowUpdate(workflow); err != nil {
		log.Error().Err(err).Int64("workflow_id", workflow.ID).
			Msg("finalize: failed to update workflow")
	}
}
