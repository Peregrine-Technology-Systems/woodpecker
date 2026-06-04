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
	"context"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog/log"

	corepipeline "go.woodpecker-ci.org/woodpecker/v3/pipeline"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
)

// maxPreemptRequeues bounds how many times a single workflow may be re-queued
// by the agent-preempt self-heal before it falls through to the kill path. A
// workflow caught in a sustained preemption storm (every spot VM preempted
// seconds after claiming it) must not loop forever on the hot completion path;
// after the cap it ends as a red status an operator restarts — the pre-#275
// behaviour, never worse. (#275)
const maxPreemptRequeues = 3

// agentPreemptRequeuesTotal counts workflows re-queued by the #275 part-2
// self-heal: an agent reported a SIGTERM-origin cancel (spot preemption), the
// workflow was an idempotent non-deploy workflow, and it was re-queued for a
// fresh agent instead of being left red. A non-zero rate quantifies how much
// spot churn the self-heal is absorbing.
var agentPreemptRequeuesTotal = promauto.NewCounter(prometheus.CounterOpts{
	Namespace: "woodpecker",
	Name:      "agent_preempt_requeues_total",
	Help:      "Workflows re-queued by the #275 agent-preempt self-heal (graceful spot preemption, idempotent non-deploy class).",
})

// maybeRequeueOnAgentShutdown attempts the #275 part-2 self-heal: re-queue a
// workflow that was killed by an agent's OWN shutdown (a graceful SIGTERM on
// spot preemption, positively signalled via Error="agent shutdown", #279)
// rather than left as a red killed status that never recovers (the pipeline
// 6041 gap). It returns true only when the workflow was actually re-queued, in
// which case the caller MUST return early from Done() without finalizing the
// workflow — the pipeline stays running and the workflow returns to pending for
// a fresh agent.
//
// Guards — all must hold, every one fails closed to the kill path:
//   - NOT deploy-class. Deploy / promote / version-bump / tag workflows are
//     forced on-demand (#266) precisely because they are not preemption-safe; a
//     half re-run of a release is worse than a red status, so they never
//     auto-re-run even defensively. (In practice an on-demand workflow is never
//     preempted, so this is belt-and-suspenders — but it is load-bearing if the
//     force-ondemand classifier ever has a gap.) sync-back is deliberately NOT
//     deploy-class (idempotent RELEASE_NOTES housekeeping) and so DOES self-heal.
//   - Under the per-workflow re-queue cap (maxPreemptRequeues).
//   - The task is still in the running set (the agent's Done RPC has not yet
//     acked it) so there is something to re-queue.
//
// Unlike ReleaseAgentTasks (the hard-WS-disconnect path), this does NOT gate on
// stepDidRealWork: the deploy-class exclusion already carries the side-effect
// safety, and the whole point of #275 is that an idempotent CI workflow with
// some already-succeeded steps (6041: clone/lint success, test killed) must
// still re-run in full. That is why it uses resetWorkflowForFullRequeue (reset
// EVERY step) rather than the partial resetWorkflowForRequeue.
func (s *RPC) maybeRequeueOnAgentShutdown(c context.Context, workflow *model.Workflow, strWorkflowID string, isTagEvent bool) bool {
	if corepipeline.ShouldForceOndemand(workflow.Name, isTagEvent, corepipeline.DeployPatterns()) {
		log.Debug().Str("workflow", workflow.Name).Int64("workflow_id", workflow.ID).
			Msg("agent-preempt self-heal: deploy-class workflow — not re-queuing (#275)")
		return false
	}

	if s.preemptRequeueCount(workflow.ID) >= maxPreemptRequeues {
		log.Warn().Str("workflow", workflow.Name).Int64("workflow_id", workflow.ID).
			Int("cap", maxPreemptRequeues).
			Msg("agent-preempt self-heal: re-queue cap reached — killing instead (#275)")
		return false
	}

	// The task must still be in the running set — the agent's Done RPC has not
	// yet acked it, so queue.Requeue can atomically move it running → pending.
	var task *model.Task
	for _, t := range s.queue.Info(c).Running {
		if t.ID == strWorkflowID {
			task = t
			break
		}
	}
	if task == nil {
		log.Warn().Str("workflow_id", strWorkflowID).
			Msg("agent-preempt self-heal: task not in running set — killing instead (#275)")
		return false
	}

	if err := s.resetWorkflowForFullRequeue(workflow); err != nil {
		log.Error().Err(err).Int64("workflow_id", workflow.ID).
			Msg("agent-preempt self-heal: full reset failed — killing instead (#275)")
		return false
	}
	if err := s.queue.Requeue(c, task); err != nil {
		log.Error().Err(err).Str("task_id", task.ID).
			Msg("agent-preempt self-heal: Requeue failed — killing instead (#275)")
		return false
	}

	s.incPreemptRequeue(workflow.ID)
	agentPreemptRequeuesTotal.Inc()
	log.Info().Str("workflow", workflow.Name).Int64("workflow_id", workflow.ID).
		Msg("agent-preempt self-heal: re-queued idempotent workflow for a fresh agent (#275)")
	return true
}

// resetWorkflowForFullRequeue returns a workflow AND every one of its steps to a
// clean pending state for a full re-run on a fresh agent. It differs from
// resetWorkflowForRequeue (which preserves already-success steps because that
// path only runs when no step did real work): the agent-preempt self-heal
// re-runs an idempotent non-deploy workflow from scratch even though earlier
// steps succeeded, so EVERY step is reset — otherwise the new agent's re-report
// of an already-success step would be rejected as a terminal-state transition.
// Re-running succeeded steps is safe for the idempotent class; deploy-class
// workflows never reach this path. (#275)
func (s *RPC) resetWorkflowForFullRequeue(wf *model.Workflow) error {
	wf.State = model.StatusPending
	wf.AgentID = 0
	wf.Started = 0
	wf.Finished = 0
	wf.Error = ""
	if err := s.store.WorkflowUpdate(wf); err != nil {
		return err
	}
	for _, step := range wf.Children {
		step.State = model.StatusPending
		step.Started = 0
		step.Finished = 0
		step.ExitCode = 0
		step.Error = ""
		if err := s.store.StepUpdate(step); err != nil {
			return err
		}
	}
	return nil
}

// preemptRequeueCount reports how many times the workflow has been re-queued by
// the self-heal in this server process. Reads are safe on a nil map.
func (s *RPC) preemptRequeueCount(workflowID int64) int {
	s.preemptRequeuesMu.Lock()
	defer s.preemptRequeuesMu.Unlock()
	return s.preemptRequeues[workflowID]
}

// incPreemptRequeue records one successful self-heal re-queue for the workflow,
// lazily allocating the map so tests that construct RPC{} directly work.
func (s *RPC) incPreemptRequeue(workflowID int64) {
	s.preemptRequeuesMu.Lock()
	defer s.preemptRequeuesMu.Unlock()
	if s.preemptRequeues == nil {
		s.preemptRequeues = make(map[int64]int)
	}
	s.preemptRequeues[workflowID]++
}

// clearPreemptRequeue releases a workflow's re-queue budget once it completes
// terminally (any non-re-queued Done), so the in-memory map does not leak and a
// later workflow re-using the id starts fresh. delete on a nil map is a no-op.
func (s *RPC) clearPreemptRequeue(workflowID int64) {
	s.preemptRequeuesMu.Lock()
	delete(s.preemptRequeues, workflowID)
	s.preemptRequeuesMu.Unlock()
}
