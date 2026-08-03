// Copyright 2022 Woodpecker Authors
// Copyright 2021 Informatyka Boguslawski sp. z o.o. sp.k., http://www.ib.pl/
// Copyright 2018 Drone.IO Inc.
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
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog/log"
	grpcMetadata "google.golang.org/grpc/metadata"

	"go.woodpecker-ci.org/woodpecker/v3/rpc"
	"go.woodpecker-ci.org/woodpecker/v3/server"
	"go.woodpecker-ci.org/woodpecker/v3/server/forge"
	"go.woodpecker-ci.org/woodpecker/v3/server/logging"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/pipeline"
	"go.woodpecker-ci.org/woodpecker/v3/server/plugin"
	"go.woodpecker-ci.org/woodpecker/v3/server/pubsub"
	"go.woodpecker-ci.org/woodpecker/v3/server/queue"
	"go.woodpecker-ci.org/woodpecker/v3/server/store"
)

// updateAgentLastWorkDelay the delay before the LastWork info should be updated.
const updateAgentLastWorkDelay = time.Minute

type RPC struct {
	queue                    queue.Queue
	pubsub                   *pubsub.Publisher
	logger                   logging.Log
	store                    store.Store
	pipelineTime             *prometheus.GaugeVec
	pipelineCount            *prometheus.CounterVec
	rpcUpdateRejectedTotal   prometheus.Counter
	ciPipelineTime           *prometheus.GaugeVec   // ci_* alias (#226)
	ciPipelineCount          *prometheus.CounterVec // ci_* alias (#226)
	ciRPCUpdateRejectedTotal prometheus.Counter     // ci_* alias (#226)
	deployPatterns           []string               // workflow names that should prefer on-demand agents
	// noScheduleOverrideLabel is the task label key (default "backend") that, when
	// its value exactly matches a NoSchedule agent's own custom label of the same
	// key, lets that specific task bypass the NoSchedule cordon (#305). Empty
	// disables the override. See loadNoScheduleOverrideLabel.
	noScheduleOverrideLabel string
	// preemptRequeues bounds the #275 agent-preempt self-heal: workflowID →
	// count of re-queues triggered by a graceful spot preemption this process.
	// In-memory by design (a restart resets the budget, at worst allowing a few
	// extra requeues of an idempotent workflow). Guarded by preemptRequeuesMu.
	preemptRequeues   map[int64]int
	preemptRequeuesMu sync.Mutex
}

// Next blocks until it provides the next workflow to execute.
// TODO (6038): Server does not release waiting agents on graceful shutdown.
func (s *RPC) Next(c context.Context, agentFilter rpc.Filter) (*rpc.Workflow, error) {
	if hostname, err := s.getHostnameFromContext(c); err == nil {
		log.Debug().Msgf("agent connected: %s: polling", hostname)
	}

	agent, err := s.getAgentFromContext(c)
	if err != nil {
		return nil, err
	}

	// A NoSchedule agent with no override-label match must not even reach the
	// queue -- same fast-path as before #305 (avoids waking the queue's Poll
	// machinery for an agent that categorically can't take general work).
	if agent.NoSchedule && !s.hasNoScheduleOverride(agent) {
		time.Sleep(1 * time.Second)
		return nil, nil
	}

	agentServerLabels, err := agent.GetServerLabels()
	if err != nil {
		return nil, err
	}

	// enforce labels from server by overwriting agent labels
	maps.Copy(agentFilter.Labels, agentServerLabels)

	// merge agent's custom labels (tier, etc.) so filter can score them
	if agent.CustomLabels != nil {
		maps.Copy(agentFilter.Labels, agent.CustomLabels)
	}

	log.Trace().Msgf("Agent %s[%d] tries to pull task with labels: %v", agent.Name, agent.ID, agentFilter.Labels)

	filterFn := createFilterFuncWithDeploy(agentFilter, s.deployPatterns)

	if agent.NoSchedule {
		// #305: NoSchedule blocks general/untargeted work, but a task that
		// EXPLICITLY host-pins to this exact agent (its task label at
		// noScheduleOverrideLabel equals this agent's own custom label of the
		// same key -- e.g. `backend: local-d3ci42`) is still allowed through.
		// This isn't a bypass of the cordon: it narrows the filter so ONLY
		// such exactly-targeted tasks can match; everything else -- including
		// tasks with no such label at all, or a different value -- keeps
		// failing exactly as before. We don't know which task (if any) is
		// pending until we ask the queue, so the gate has to live in the
		// filter function, not as an early return.
		//
		// #346: the override match must SKIP the base filter entirely, not
		// layer on top of it. baseFilterFn (createFilterFuncWithDeploy) still
		// requires every task label key to have a matching or wildcarded entry
		// in the agent's own filter labels -- a deliberately minimal pinned
		// agent (e.g. WOODPECKER_FILTER_LABELS=backend=local-d3ci42 only) fails
		// that requirement for any task carrying auto-injected labels the
		// override never asked about (tier, org-id, woodpecker-ci.org/branch,
		// forge-id, repo-id, ...) -- which is effectively every real task. An
		// exact override match is a stronger, more specific signal than the
		// base filter's per-label check, so it wins outright rather than being
		// additionally gated by it.
		overrideValue := agent.CustomLabels[s.noScheduleOverrideLabel]
		filterFn = func(task *model.Task) (bool, int) {
			if task.Labels[s.noScheduleOverrideLabel] != overrideValue {
				return false, 0
			}
			return true, 10
		}
	}

	for {
		// poll blocks until a task is available or the context is canceled / worker is kicked
		task, err := s.queue.Poll(c, agent.ID, filterFn)
		if err != nil || task == nil {
			return nil, err
		}

		if task.ShouldRun() {
			// #268: observe-only defense-in-depth behind #266. If a spot agent is
			// claiming a deploy-class task, the submit-time tier rewrite was
			// bypassed — emit a bus telemetry signal (does not refuse the task).
			s.emitTierBypassIfSpotDeploy(agent, task)
			workflow := new(rpc.Workflow)
			err = json.Unmarshal(task.Data, workflow)
			return workflow, err
		}

		log.Debug().Str("task_id", task.ID).Strs("run_on", task.RunOn).Any("dep_status", task.DepStatus).Msg("ShouldRun: false — skipping task (#162)")

		// task should not run, so mark it as done
		if err := s.Done(c, task.ID, rpc.WorkflowState{}); err != nil {
			log.Error().Err(err).Msgf("marking workflow task '%s' as done failed", task.ID)
		}
	}
}

// hasNoScheduleOverride reports whether agent carries a non-empty custom label
// at s.noScheduleOverrideLabel, i.e. whether it's even possible for some task to
// explicitly host-pin to this agent and bypass its NoSchedule cordon (#305). An
// agent with no such label (or an empty value) can never be explicitly targeted,
// so NoSchedule blocks it unconditionally -- same as before #305.
func (s *RPC) hasNoScheduleOverride(agent *model.Agent) bool {
	if s.noScheduleOverrideLabel == "" || agent.CustomLabels == nil {
		return false
	}
	return agent.CustomLabels[s.noScheduleOverrideLabel] != ""
}

// Wait blocks until the workflow with the given ID is completed or got canceled.
// Used to let agents wait for cancel signals from server side.
func (s *RPC) Wait(c context.Context, workflowID string) (canceled bool, err error) {
	agent, err := s.getAgentFromContext(c)
	if err != nil {
		return false, err
	}

	if err := s.checkAgentPermissionByWorkflow(c, agent, workflowID, nil, nil); err != nil {
		return false, err
	}

	if err := s.queue.Wait(c, workflowID); err != nil {
		if errors.Is(err, queue.ErrCancel) {
			// we explicit send a cancel signal
			return true, nil
		}
		// unknown error happened
		return false, err
	}

	// workflow finished and on issues appeared
	return false, nil
}

// Extend extends the lease for the workflow with the given ID.
func (s *RPC) Extend(c context.Context, workflowID string) error {
	agent, err := s.getAgentFromContext(c)
	if err != nil {
		return err
	}

	err = s.updateAgentLastWork(agent)
	if err != nil {
		return err
	}

	if err := s.checkAgentPermissionByWorkflow(c, agent, workflowID, nil, nil); err != nil {
		return err
	}

	return s.queue.Extend(c, agent.ID, workflowID)
}

// ReleaseAgentTasks releases all running tasks assigned to a disconnecting agent (#3, #4, #8).
// Called on EVERY WebSocket disconnect (clean or unexpected) — releases tasks
// immediately instead of waiting for TaskTimeout (15 minutes).
// Updates the queue, workflow, steps, parent pipeline, and forge status (GitHub).
//
// Tasks are split by whether execution had started (#72):
//   - workflow.Started == 0: agent claimed the task but never began executing it.
//     Safe to re-queue — another agent can pick it up with no work lost.
//   - workflow.Started > 0: agent was mid-execution. Kill as before.
func (s *RPC) ReleaseAgentTasks(c context.Context, agentID int64) {
	info := s.queue.Info(c)

	// Index running tasks by ID for O(1) lookup when re-queuing.
	taskByID := make(map[string]*model.Task, len(info.Running))
	var orphaned []string
	for _, task := range info.Running {
		if task.AgentID == agentID {
			orphaned = append(orphaned, task.ID)
			taskByID[task.ID] = task
		}
	}

	// #8: Log running tasks with mismatched or zero AgentID for debugging ghost tasks
	if len(orphaned) == 0 && len(info.Running) > 0 {
		for _, task := range info.Running {
			log.Debug().Int64("agent_id", agentID).Int64("task_agent_id", task.AgentID).
				Str("task_id", task.ID).Str("task_name", task.Name).
				Msg("release: running task not matched — possible ghost")
		}
	}

	if len(orphaned) == 0 {
		return
	}
	log.Warn().Int64("agent_id", agentID).Int("tasks", len(orphaned)).
		Msg("releasing tasks from disconnecting agent")

	// Partition into re-queue vs kill using step execution as the signal. (#72,#224,#225)
	// wf.Started > 0 is insufficient — Init sets Started before any step runs.
	// A workflow with all steps pending (even after Init) is safe to re-queue.
	var toKill []string
	var toRequeue []*model.Task
	requeueWF := make(map[string]*model.Workflow) // workflowIDStr → wf for Init-called workflows
	workflowCache := make(map[string]*model.Workflow, len(orphaned))
	for _, workflowIDStr := range orphaned {
		workflowID, err := strconv.ParseInt(workflowIDStr, 10, 64)
		if err != nil {
			toKill = append(toKill, workflowIDStr)
			continue
		}
		wf, err := s.store.WorkflowLoad(workflowID)
		if err != nil {
			toKill = append(toKill, workflowIDStr)
			continue
		}

		steps, _ := s.store.StepListFromWorkflowFind(wf)
		// #235 F3: re-queue is safe only when NO step did observable work.
		// A step that was merely claimed and stamped Started but produced no
		// output and never returned a real exit code (the "agent ack'd then
		// vanished" shape from #1327 — e.g. a spot VM preempted seconds after
		// dispatch-ack) did nothing re-runnable. Re-running a step that DID
		// work would duplicate side effects (a partial deploy), so any such
		// step forces the kill path. This widens the previous predicate, which
		// killed on Started>0 alone and so left #1327's phantom-started
		// workflows unrecoverable.
		didRealWork := false
		for _, step := range steps {
			if s.stepDidRealWork(step) {
				didRealWork = true
				break
			}
		}

		if didRealWork {
			wf.Children = steps // cache so kill loop skips a second load
			workflowCache[workflowIDStr] = wf
			toKill = append(toKill, workflowIDStr)
			continue
		}

		// No step did real work — safe to re-queue.
		task := taskByID[workflowIDStr]
		if task == nil {
			// Task not in running map for this agent — fall back to kill (#225).
			log.Warn().Int64("agent_id", agentID).Int64("workflow_id", workflowID).
				Msg("release: task not in running map — falling back to kill")
			wf.Children = steps
			workflowCache[workflowIDStr] = wf
			toKill = append(toKill, workflowIDStr)
			continue
		}

		log.Info().Int64("agent_id", agentID).Int64("workflow_id", workflowID).
			Bool("had_init", wf.Started > 0).Int("steps", len(steps)).
			Msg("release: re-queuing claimed-but-no-work-done task (#224/#1327)")
		toRequeue = append(toRequeue, task)
		wf.Children = steps
		requeueWF[workflowIDStr] = wf
	}

	// Re-queue tasks that did no real work. Reset DB state to pending — both
	// the workflow (if Init ran, wf.Started > 0) and any phantom-started steps
	// — so the UI is accurate while the task waits for a new agent and so the
	// new agent's step updates aren't rejected as terminal-state transitions.
	// Use Requeue (not PushAtOnce) to atomically remove from q.running and
	// insert into q.pending (#225).
	for _, task := range toRequeue {
		if wf := requeueWF[task.ID]; wf != nil {
			if err := s.resetWorkflowForRequeue(wf); err != nil {
				log.Error().Err(err).Int64("workflow_id", wf.ID).
					Msg("release: failed to reset state for re-queue — killing instead")
				toKill = append(toKill, task.ID)
				continue
			}
		}
		if err := s.queue.Requeue(c, task); err != nil {
			log.Error().Err(err).Str("task_id", task.ID).
				Msg("release: Requeue failed — killing instead")
			toKill = append(toKill, task.ID)
		}
	}

	// Kill tasks that had actually started executing.
	if err := s.queue.ErrorAtOnce(c, toKill, queue.ErrCancel); err != nil {
		log.Error().Err(err).Msg("failed to release agent tasks from queue")
	}

	// Update pipeline database — mirror the completion logic from rpc.Done()
	// so the pipeline reaches a terminal state and GitHub gets notified (#4).
	// Only kill DB records for tasks that were actually running.
	for _, workflowIDStr := range toKill {
		workflowID, err := strconv.ParseInt(workflowIDStr, 10, 64)
		if err != nil {
			continue
		}

		// Use cached workflow from partition step if available.
		workflow, ok := workflowCache[workflowIDStr]
		if !ok {
			workflow, err = s.store.WorkflowLoad(workflowID)
			if err != nil {
				log.Error().Err(err).Int64("workflow_id", workflowID).
					Msg("release: failed to load workflow")
				continue
			}
		}

		// Load children (steps) so WorkflowStatus can evaluate them.
		// Skip if already cached from the partition step (avoids a second DB hit).
		if workflow.Children == nil {
			workflow.Children, err = s.store.StepListFromWorkflowFind(workflow)
			if err != nil {
				log.Error().Err(err).Int64("workflow_id", workflowID).
					Msg("release: failed to load workflow steps")
			}
		}

		now := time.Now().Unix()

		// Mark pending/running steps as killed. Track whether any steps
		// were actually in-flight — if all steps already completed, the
		// workflow should reflect their outcome, not be marked killed (#168).
		anyKilled := false
		for _, step := range workflow.Children {
			if step.Running() || step.State == model.StatusPending {
				step.State = model.StatusKilled
				step.Finished = now
				step.Error = "agent disconnected"
				if err := s.store.StepUpdate(step); err != nil {
					log.Error().Err(err).Int64("step_id", step.ID).
						Msg("release: failed to kill step")
				}
				anyKilled = true
			}
		}

		// #235: a step killed by the disconnect that declares an
		// outcome-verification proof-query gets one read-only probe — covers
		// the "agent disconnected mid-deploy but the deploy actually landed"
		// case (peregrine-ci-scaler#1055). A reconciled step flips to success,
		// so the workflow status below is computed from the real outcome.
		if n := pipeline.ReconcileVerifiedKilledSteps(c, s.store, workflow); n > 0 {
			log.Info().Int64("workflow_id", workflowID).Int("reconciled_steps", n).
				Msg("verify: reconciled disconnect-killed step(s) to success (#235)")
			anyKilled = false
			for _, st := range workflow.Children {
				if st.State == model.StatusKilled {
					anyKilled = true
					break
				}
			}
		}

		// If all steps had already completed before the disconnect, compute
		// workflow status from them instead of marking killed (#168).
		workflow.Finished = now
		if anyKilled {
			workflow.State = model.StatusKilled
			workflow.Error = "agent disconnected"
		} else {
			workflow.State = pipeline.WorkflowStatus(workflow.Children)
		}
		if err := s.store.WorkflowUpdate(workflow); err != nil {
			log.Error().Err(err).Int64("workflow_id", workflowID).
				Msg("release: failed to update workflow on disconnect")
		}

		// Update parent pipeline status
		currentPipeline, err := s.store.GetPipeline(workflow.PipelineID)
		if err != nil {
			log.Error().Err(err).Int64("pipeline_id", workflow.PipelineID).
				Msg("release: failed to load pipeline")
			continue
		}

		currentPipeline.Workflows, err = s.store.WorkflowGetTree(currentPipeline)
		if err != nil {
			log.Error().Err(err).Int64("pipeline_id", currentPipeline.ID).
				Msg("release: failed to load workflow tree")
			continue
		}

		if !model.IsThereRunningStage(currentPipeline.Workflows) {
			if _, err := pipeline.UpdateStatusToDone(s.store, *currentPipeline, pipeline.PipelineStatus(currentPipeline.Workflows), now); err != nil {
				log.Error().Err(err).Int64("pipeline_id", currentPipeline.ID).
					Msg("release: failed to finalize pipeline")
			}
		}

		// Push status to GitHub so PR checks update
		repo, err := s.store.GetRepo(currentPipeline.RepoID)
		if err != nil {
			log.Error().Err(err).Int64("repo_id", currentPipeline.RepoID).
				Msg("release: failed to load repo")
			continue
		}
		s.updateForgeStatus(c, repo, currentPipeline, workflow)
	}
}

// stepDidRealWork reports whether a step executed observable work, which makes
// its workflow unsafe to re-queue (re-running would duplicate side effects such
// as a partial deploy). A step that was merely claimed and stamped Started but
// produced no log output and never returned a real exit code — the "agent
// ack'd dispatch then vanished" shape from #1327 / pts.413 — did nothing
// re-runnable and is safe to re-queue. (#235 F3)
func (s *RPC) stepDidRealWork(step *model.Step) bool {
	if step.Started == 0 {
		return false // never dispatched to the runtime
	}
	// Completed successfully, or ran and returned a real (positive) exit code.
	// ExitCode <= 0 covers "never exited" (-1) and the not-yet-set zero value.
	if step.State == model.StatusSuccess || step.ExitCode > 0 {
		return true
	}
	// No terminal real-exit signal — fall back to log output. A step that
	// emitted even one log line had started doing observable work.
	logs, err := s.store.LogFind(step)
	if err != nil {
		// Can't prove the step was a no-op — be conservative and treat it as
		// real work so a possibly-side-effecting step is never re-run.
		log.Warn().Err(err).Int64("step_id", step.ID).
			Msg("release: LogFind failed — treating step as real work")
		return true
	}
	return len(logs) > 0
}

// resetWorkflowForRequeue returns a re-queued workflow and its phantom-started
// steps to a clean pending state so the UI is accurate while the task waits for
// a new agent, and so the new agent's step updates are not rejected as
// terminal-state transitions. Only invoked for workflows where no step did real
// work (see stepDidRealWork), so resetting non-success steps is safe. (#235 F3)
func (s *RPC) resetWorkflowForRequeue(wf *model.Workflow) error {
	if wf.Started > 0 {
		wf.State = model.StatusPending
		wf.AgentID = 0
		wf.Started = 0
		if err := s.store.WorkflowUpdate(wf); err != nil {
			return err
		}
	}
	for _, step := range wf.Children {
		if step.State == model.StatusSuccess || (step.Started == 0 && step.State == model.StatusPending) {
			continue // nothing to reset
		}
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

// Update let agent updates the step state at the server.
func (s *RPC) Update(c context.Context, strWorkflowID string, state rpc.StepState) error {
	workflowID, err := strconv.ParseInt(strWorkflowID, 10, 64)
	if err != nil {
		return err
	}

	workflow, err := s.store.WorkflowLoad(workflowID)
	if err != nil {
		log.Error().Err(err).Msgf("rpc.update: cannot find workflow with id %d", workflowID)
		return err
	}

	currentPipeline, err := s.store.GetPipeline(workflow.PipelineID)
	if err != nil {
		log.Error().Err(err).Msgf("cannot find pipeline with id %d", workflow.PipelineID)
		return err
	}

	agent, err := s.getAgentFromContext(c)
	if err != nil {
		return err
	}

	step, err := s.store.StepByUUID(state.StepUUID)
	if err != nil {
		log.Error().Err(err).Msgf("cannot find step with uuid %s", state.StepUUID)
		return err
	}

	if step.PipelineID != currentPipeline.ID {
		msg := fmt.Sprintf("agent returned status with step uuid '%s' which does not belong to current pipeline", state.StepUUID)
		log.Error().
			Int64("stepPipelineID", step.PipelineID).
			Int64("currentPipelineID", currentPipeline.ID).
			Msg(msg)
		return errors.New(msg)
	}

	repo, err := s.store.GetRepo(currentPipeline.RepoID)
	if err != nil {
		log.Error().Err(err).Msgf("cannot find repo with id %d", currentPipeline.RepoID)
		return err
	}

	// check before agent can alter some state
	if err := s.checkAgentPermissionByWorkflow(c, agent, strWorkflowID, currentPipeline, repo); err != nil {
		return err
	}

	if err := pipeline.UpdateStepStatus(c, s.store, step, state); err != nil {
		// #31: if the step is in a terminal state server-side, propagate the
		// sentinel back to the gRPC layer so it can return FailedPrecondition.
		// The agent uses that to stop sending updates for abandoned work.
		// Other (unknown-state) errors stay as log-only — historical behavior.
		if errors.Is(err, pipeline.ErrStepUpdateRejectedTerminal) {
			if s.rpcUpdateRejectedTotal != nil {
				s.rpcUpdateRejectedTotal.Inc()
			}
			if s.ciRPCUpdateRejectedTotal != nil {
				s.ciRPCUpdateRejectedTotal.Inc()
			}
			log.Warn().
				Err(err).
				Str("step_uuid", state.StepUUID).
				Int64("agent_id", agent.ID).
				Msg("rpc.update: rejecting update for step in terminal state — agent should stop")
			return err
		}
		log.Error().Err(err).Msg("rpc.update: cannot update step")
	}

	if state.Exited {
		pipeline.EmitEvent(plugin.EventStepCompleted, repo, currentPipeline, step.Name)
		server.Config.Services.LogStore.StepFinished(step)
	}

	if currentPipeline.Workflows, err = s.store.WorkflowGetTree(currentPipeline); err != nil {
		log.Error().Err(err).Msg("cannot build tree from step list")
		return err
	}
	message := pubsub.Message{
		Labels: map[string]string{
			"repo":    repo.FullName,
			"private": strconv.FormatBool(repo.IsSCMPrivate),
		},
	}
	message.Data, err = json.Marshal(model.Event{
		Repo:     *repo,
		Pipeline: *currentPipeline,
	})
	if err != nil {
		return err
	}
	s.pubsub.Publish(message)

	return nil
}

// Init signals the workflow is initialized.
func (s *RPC) Init(c context.Context, strWorkflowID string, state rpc.WorkflowState) error {
	workflowID, err := strconv.ParseInt(strWorkflowID, 10, 64)
	if err != nil {
		return err
	}

	workflow, err := s.store.WorkflowLoad(workflowID)
	if err != nil {
		log.Error().Err(err).Msgf("cannot find workflow with id %d", workflowID)
		return err
	}

	agent, err := s.getAgentFromContext(c)
	if err != nil {
		return err
	}

	workflow.AgentID = agent.ID

	currentPipeline, err := s.store.GetPipeline(workflow.PipelineID)
	if err != nil {
		log.Error().Err(err).Msgf("cannot find pipeline with id %d", workflow.PipelineID)
		return err
	}

	repo, err := s.store.GetRepo(currentPipeline.RepoID)
	if err != nil {
		log.Error().Err(err).Msgf("cannot find repo with id %d", currentPipeline.RepoID)
		return err
	}

	// check before agent can alter some state
	if err := s.checkAgentPermissionByWorkflow(c, agent, strWorkflowID, currentPipeline, repo); err != nil {
		return err
	}

	if currentPipeline.Status == model.StatusPending {
		if currentPipeline, err = pipeline.UpdateToStatusRunning(s.store, *currentPipeline, state.Started); err != nil {
			log.Error().Err(err).Msgf("init: cannot update pipeline %d state", currentPipeline.ID)
		}
		pipeline.EmitEvent(plugin.EventPipelineStarted, repo, currentPipeline, "")
	}

	s.updateForgeStatus(c, repo, currentPipeline, workflow)

	defer func() {
		currentPipeline.Workflows, _ = s.store.WorkflowGetTree(currentPipeline)
		message := pubsub.Message{
			Labels: map[string]string{
				"repo":    repo.FullName,
				"private": strconv.FormatBool(repo.IsSCMPrivate),
			},
		}
		message.Data, err = json.Marshal(model.Event{
			Repo:     *repo,
			Pipeline: *currentPipeline,
		})
		if err != nil {
			log.Error().Err(err).Msg("could not marshal JSON")
			return
		}
		s.pubsub.Publish(message)
	}()

	workflow, err = pipeline.UpdateWorkflowStatusToRunning(s.store, *workflow, state)
	if err != nil {
		return err
	}
	s.updateForgeStatus(c, repo, currentPipeline, workflow)

	return s.updateAgentLastWork(agent)
}

// Done marks the workflow with the given ID as stope.
func (s *RPC) Done(c context.Context, strWorkflowID string, state rpc.WorkflowState) error {
	workflowID, err := strconv.ParseInt(strWorkflowID, 10, 64)
	if err != nil {
		return err
	}

	workflow, err := s.store.WorkflowLoad(workflowID)
	if err != nil {
		log.Error().Err(err).Msgf("cannot find workflow with id %d", workflowID)
		return err
	}

	workflow.Children, err = s.store.StepListFromWorkflowFind(workflow)
	if err != nil {
		return err
	}

	currentPipeline, err := s.store.GetPipeline(workflow.PipelineID)
	if err != nil {
		log.Error().Err(err).Msgf("cannot find pipeline with id %d", workflow.PipelineID)
		return err
	}

	repo, err := s.store.GetRepo(currentPipeline.RepoID)
	if err != nil {
		log.Error().Err(err).Msgf("cannot find repo with id %d", currentPipeline.RepoID)
		return err
	}

	agent, err := s.getAgentFromContext(c)
	if err != nil {
		return err
	}

	// check before agent can alter some state
	if err := s.checkAgentPermissionByWorkflow(c, agent, strWorkflowID, currentPipeline, repo); err != nil {
		return err
	}

	logger := log.With().
		Str("repo_id", fmt.Sprint(repo.ID)).
		Str("pipeline_id", fmt.Sprint(currentPipeline.ID)).
		Str("workflow_id", strWorkflowID).Logger()

	logger.Debug().Msgf("workflow state in store: %#v", workflow)
	logger.Debug().Msgf("gRPC Done with state: %#v", state)

	// #275 part 2: graceful spot-preemption self-heal. When the agent reports a
	// cancel that originated from ITS OWN shutdown (SIGTERM on spot preemption,
	// positively signalled via Error="agent shutdown", #279) — not a
	// server-issued cancel (supersede / user / pending-only, which report a
	// plain "Canceled") — re-queue an idempotent non-deploy workflow for a fresh
	// agent instead of leaving a red killed status that never self-heals (the
	// 6041 gap). Hooked BEFORE the workflow is finalized: a successful re-queue
	// returns early, leaving the pipeline running and the workflow back in
	// pending. Every guard inside fails closed to the normal finalize-as-killed
	// path below, so this can only ever turn a would-be-red into a re-run.
	if state.Canceled && model.IsAgentShutdownError(state.Error) {
		isTagEvent := currentPipeline.Event == model.EventTag
		if s.maybeRequeueOnAgentShutdown(c, workflow, strWorkflowID, isTagEvent) {
			return s.updateAgentLastWork(agent)
		}
	}
	// Reached here ⇒ this workflow is completing terminally (not re-queued).
	// Release any self-heal budget it accumulated so the map does not leak and a
	// later re-use of the workflow id starts fresh. (#275)
	s.clearPreemptRequeue(workflow.ID)

	// Mark pending/running children as skipped before computing workflow status,
	// so steps with when:status:[failure] that were never dispatched don't keep
	// the workflow stuck in "pending" state.
	s.completeChildrenIfParentCompleted(workflow)
	workflow.Children, err = s.store.StepListFromWorkflowFind(workflow)
	if err != nil {
		return err
	}

	// #235: a step killed mid-execution that declares an outcome-verification
	// proof-query gets one read-only probe; if it confirms the work landed
	// (e.g. the deploy target's /version reports the expected commit), the step
	// is reconciled to success. Combined with the #230/#232 invariant below,
	// flipping the only killed step to success makes the workflow succeed.
	if n := pipeline.ReconcileVerifiedKilledSteps(c, s.store, workflow); n > 0 {
		logger.Info().Int("reconciled_steps", n).Msg("verify: reconciled killed step(s) to success (#235)")
	}

	if workflow, err = pipeline.UpdateWorkflowStatusToDone(s.store, *workflow, state); err != nil {
		logger.Error().Err(err).Msgf("pipeline.UpdateWorkflowStatusToDone: cannot update workflow state: %s", err)
	}

	var queueErr error
	switch {
	case !state.Canceled:
		if workflow.Failing() {
			queueErr = s.queue.Error(c, strWorkflowID, fmt.Errorf("workflow finished with error %s", state.Error))
		} else {
			queueErr = s.queue.Done(c, strWorkflowID, workflow.State)
		}
	// #230/#232: the agent reported Canceled, but UpdateWorkflowStatusToDone
	// may have reconciled the workflow to its real terminal outcome when the
	// cancel arrived after every step had already finished. Propagate that
	// reconciled state to queue dependents instead of blindly killing them, so
	// a downstream workflow that depends_on a reconciled-success workflow still
	// runs.
	case workflow.State != model.StatusKilled && workflow.State != model.StatusCanceled:
		queueErr = s.queue.Done(c, strWorkflowID, workflow.State)
	case workflow.Started > 0:
		queueErr = s.queue.Done(c, strWorkflowID, model.StatusKilled)
	default:
		queueErr = s.queue.Done(c, strWorkflowID, model.StatusCanceled)
	}
	if queueErr != nil {
		logger.Error().Err(queueErr).Msg("queue.Done: cannot ack workflow")
	}

	currentPipeline.Workflows, err = s.store.WorkflowGetTree(currentPipeline)
	if err != nil {
		return err
	}

	if !model.IsThereRunningStage(currentPipeline.Workflows) {
		if currentPipeline, err = pipeline.UpdateStatusToDone(s.store, *currentPipeline, pipeline.PipelineStatus(currentPipeline.Workflows), workflow.Finished); err != nil {
			logger.Error().Err(err).Msgf("pipeline.UpdateStatusToDone: cannot update workflows final state")
		}
		pipeline.EmitEvent(plugin.EventPipelineCompleted, repo, currentPipeline, "")
	}

	s.updateForgeStatus(c, repo, currentPipeline, workflow)

	// make sure writes to pubsub are non blocking (https://github.com/woodpecker-ci/woodpecker/blob/c919f32e0b6432a95e1a6d3d0ad662f591adf73f/server/logging/log.go#L9)
	go func() {
		for _, step := range workflow.Children {
			if err := s.logger.Close(c, step.ID); err != nil {
				logger.Error().Err(err).Msgf("done: cannot close log stream for step %d", step.ID)
			}
		}
	}()

	if err := s.notify(repo, currentPipeline); err != nil {
		return err
	}

	if currentPipeline.Status == model.StatusSuccess || currentPipeline.Status == model.StatusFailure {
		s.pipelineCount.WithLabelValues(repo.FullName, currentPipeline.Branch, string(currentPipeline.Status), "total").Inc()
		s.pipelineTime.WithLabelValues(repo.FullName, currentPipeline.Branch, string(currentPipeline.Status), "total").Set(float64(currentPipeline.Finished - currentPipeline.Started))
		s.ciPipelineCount.WithLabelValues(repo.FullName, currentPipeline.Branch, string(currentPipeline.Status), "total").Inc()
		s.ciPipelineTime.WithLabelValues(repo.FullName, currentPipeline.Branch, string(currentPipeline.Status), "total").Set(float64(currentPipeline.Finished - currentPipeline.Started))
	}
	if currentPipeline.IsMultiPipeline() {
		s.pipelineTime.WithLabelValues(repo.FullName, currentPipeline.Branch, string(workflow.State), workflow.Name).Set(float64(workflow.Finished - workflow.Started))
		s.ciPipelineTime.WithLabelValues(repo.FullName, currentPipeline.Branch, string(workflow.State), workflow.Name).Set(float64(workflow.Finished - workflow.Started))
	}

	return s.updateAgentLastWork(agent)
}

// Log writes a log entry to the database and publishes it to the pubsub.
// An explicit stepUUID makes it obvious that all entries must come from the same step.
func (s *RPC) Log(c context.Context, stepUUID string, rpcLogEntries []*rpc.LogEntry) error {
	step, err := s.store.StepByUUID(stepUUID)
	if err != nil {
		return fmt.Errorf("could not find step with uuid %s in store: %w", stepUUID, err)
	}

	agent, err := s.getAgentFromContext(c)
	if err != nil {
		return err
	}

	currentPipeline, err := s.store.GetPipeline(step.PipelineID)
	if err != nil {
		log.Error().Err(err).Msgf("cannot find pipeline with id %d", step.PipelineID)
		return err
	}

	// check before agent can alter some state
	if err := s.checkAgentPermissionByWorkflow(c, agent, "", currentPipeline, nil); err != nil {
		return err
	}

	err = s.updateAgentLastWork(agent)
	if err != nil {
		return err
	}

	var logEntries []*model.LogEntry

	for _, rpcLogEntry := range rpcLogEntries {
		if rpcLogEntry.StepUUID != stepUUID {
			return fmt.Errorf("expected step UUID %s, got %s", stepUUID, rpcLogEntry.StepUUID)
		}
		logEntries = append(logEntries, &model.LogEntry{
			StepID: step.ID,
			Time:   rpcLogEntry.Time,
			Line:   rpcLogEntry.Line,
			Data:   rpcLogEntry.Data,
			Type:   model.LogEntryType(rpcLogEntry.Type),
		})
	}

	// make sure writes to pubsub are non blocking (https://github.com/woodpecker-ci/woodpecker/blob/c919f32e0b6432a95e1a6d3d0ad662f591adf73f/server/logging/log.go#L9)
	go func() {
		// write line to listening web clients
		if err := s.logger.Write(c, step.ID, logEntries); err != nil {
			log.Error().Err(err).Msgf("rpc server could not write to logger")
		}
	}()

	if err = server.Config.Services.LogStore.LogAppend(step, logEntries); err != nil {
		log.Error().Err(err).Msg("could not store log entries")
	}

	// #233: best-effort parallel drain to GCP Cloud Logging. SQLite above is
	// the source of truth — this never affects the store write or the ack. The
	// repo lookup (for the full-name label) and the drain are gated on Enabled
	// so the disabled path adds nothing to the hot log path.
	if drain := server.Config.Services.LogDrain; drain.Enabled() {
		if repo, rerr := s.store.GetRepo(currentPipeline.RepoID); rerr == nil {
			drain.Append(repo.FullName, currentPipeline.Number, step, logEntries)
		} else {
			log.Warn().Err(rerr).Int64("repo_id", currentPipeline.RepoID).
				Msg("log drain: repo lookup failed — skipping drain for this batch")
		}
	}

	return nil
}

func (s *RPC) RegisterAgent(ctx context.Context, info rpc.AgentInfo) (int64, error) {
	agent, err := s.getAgentFromContext(ctx)
	if err != nil {
		return -1, err
	}

	if agent.Name == "" {
		if hostname, err := s.getHostnameFromContext(ctx); err == nil {
			agent.Name = hostname
		}
	}

	agent.Backend = info.Backend
	agent.Platform = info.Platform
	agent.Capacity = int32(info.Capacity)
	agent.Version = info.Version
	agent.CustomLabels = info.CustomLabels

	err = s.store.AgentUpdate(agent)
	if err != nil {
		return -1, err
	}

	return agent.ID, nil
}

// UnregisterAgent removes the agent from the database.
func (s *RPC) UnregisterAgent(ctx context.Context) error {
	agent, err := s.getAgentFromContext(ctx)
	if !agent.IsSystemAgent() {
		// registered with individual agent token -> do not unregister
		return nil
	}
	log.Debug().Msgf("un-registering agent with ID %d", agent.ID)
	if err != nil {
		return err
	}

	err = s.store.AgentDelete(agent)

	return err
}

// RemoveAgent deletes a positively-dead agent's registration row by id (#283).
// Called from the WS reconnect-grace-expiry path once an agent has failed to
// reconnect within the #208 grace: the server has already concluded it is gone
// and reclaimed its tasks, so the lingering registration is an orphan — remove
// it now (the obvious-disconnect fast-path) instead of waiting for the #254
// last_contact reaper. Mirrors UnregisterAgent's guard: only SYSTEM agents
// (auto-registered fleet) are deleted; an individually-tokened, pre-provisioned
// agent is preserved. Best-effort — a not-found / non-system / delete error
// just leaves the row for the reaper.
func (s *RPC) RemoveAgent(agentID int64) error {
	agent, err := s.store.AgentFind(agentID)
	if err != nil {
		return err
	}
	if !agent.IsSystemAgent() {
		return nil
	}
	return s.store.AgentDelete(agent)
}

func (s *RPC) ReportHealth(ctx context.Context, status string) error {
	agent, err := s.getAgentFromContext(ctx)
	if err != nil {
		return err
	}

	if status != "I am alive!" {
		//nolint:staticcheck
		return errors.New("Are you alive?")
	}

	agent.LastContact = time.Now().Unix()

	return s.store.AgentUpdate(agent)
}

func (s *RPC) checkAgentPermissionByWorkflow(_ context.Context, agent *model.Agent, strWorkflowID string, pipeline *model.Pipeline, repo *model.Repo) error {
	var err error
	if repo == nil && pipeline == nil {
		workflowID, err := strconv.ParseInt(strWorkflowID, 10, 64)
		if err != nil {
			return err
		}

		workflow, err := s.store.WorkflowLoad(workflowID)
		if err != nil {
			log.Error().Err(err).Msgf("cannot find workflow with id %d", workflowID)
			return err
		}

		pipeline, err = s.store.GetPipeline(workflow.PipelineID)
		if err != nil {
			log.Error().Err(err).Msgf("cannot find pipeline with id %d", workflow.PipelineID)
			return err
		}
	}

	if repo == nil {
		repo, err = s.store.GetRepo(pipeline.RepoID)
		if err != nil {
			log.Error().Err(err).Msgf("cannot find repo with id %d", pipeline.RepoID)
			return err
		}
	}

	if agent.CanAccessRepo(repo) {
		return nil
	}

	msg := fmt.Sprintf("agent '%d' is not allowed to interact with repo[%d] '%s'", agent.ID, repo.ID, repo.FullName)
	log.Error().Int64("repoId", repo.ID).Msg(msg)
	return errors.New(msg)
}

func (s *RPC) completeChildrenIfParentCompleted(completedWorkflow *model.Workflow) {
	for _, c := range completedWorkflow.Children {
		if c.Running() || c.State == model.StatusPending {
			if _, err := pipeline.UpdateStepToStatusSkipped(s.store, *c, completedWorkflow.Finished, model.StatusSkipped); err != nil {
				log.Error().Err(err).Msgf("done: cannot update step_id %d child state", c.ID)
			}
		}
	}
}

func (s *RPC) updateForgeStatus(ctx context.Context, repo *model.Repo, pipeline *model.Pipeline, workflow *model.Workflow) {
	// fork#44: don't post errored to GitHub on agent-disconnect kills. They
	// are infrastructure failures, not code failures, and an errored status
	// blocks branch-protection-gated merges with "Required status check ...
	// is errored" — even gh pr merge --admin can't override that. Skipping
	// the post leaves the previous status valid; the auto-requeue path
	// (peregrine-ci-scaler#741) re-runs the pipeline and posts fresh status.
	if workflow != nil && workflow.KilledByAgentDisconnect() {
		log.Info().
			Str("repo", repo.FullName).
			Int64("pipeline_id", pipeline.ID).
			Int64("workflow_id", workflow.ID).
			Str("error", workflow.Error).
			Msg("forge.Status: skipping post for agent-disconnect kill (#44)")
		return
	}

	user, err := s.store.GetUser(repo.UserID)
	if err != nil {
		log.Error().Err(err).Msgf("cannot get user with id '%d'", repo.UserID)
		return
	}

	_forge, err := server.Config.Services.Manager.ForgeFromRepo(repo)
	if err != nil {
		log.Error().Err(err).Msgf("can not get forge for repo '%s'", repo.FullName)
		return
	}

	forge.Refresh(ctx, _forge, s.store, user)

	// only do status updates for parent steps
	if workflow != nil {
		err = _forge.Status(ctx, user, repo, pipeline, workflow)
		if err != nil {
			log.Error().Err(err).Msgf("error setting commit status for %s/%d", repo.FullName, pipeline.Number)
		}
	}
}

func (s *RPC) notify(repo *model.Repo, pipeline *model.Pipeline) (err error) {
	message := pubsub.Message{
		Labels: map[string]string{
			"repo":    repo.FullName,
			"private": strconv.FormatBool(repo.IsSCMPrivate),
		},
	}
	message.Data, err = json.Marshal(model.Event{
		Repo:     *repo,
		Pipeline: *pipeline,
	})
	if err != nil {
		return err
	}
	s.pubsub.Publish(message)
	return nil
}

func (s *RPC) getAgentFromContext(ctx context.Context) (*model.Agent, error) {
	md, ok := grpcMetadata.FromIncomingContext(ctx)
	if !ok {
		return nil, errors.New("metadata is not provided")
	}

	values := md["agent_id"]
	if len(values) == 0 {
		return nil, errors.New("agent_id is not provided")
	}

	_agentID := values[0]
	agentID, err := strconv.ParseInt(_agentID, 10, 64)
	if err != nil {
		return nil, errors.New("agent_id is not a valid integer")
	}

	return s.store.AgentFind(agentID)
}

func (s *RPC) getHostnameFromContext(ctx context.Context) (string, error) {
	metadata, ok := grpcMetadata.FromIncomingContext(ctx)
	if ok {
		hostname, ok := metadata["hostname"]
		if ok && len(hostname) != 0 {
			return hostname[0], nil
		}
	}
	return "", errors.New("no hostname in metadata")
}

func (s *RPC) updateAgentLastWork(agent *model.Agent) error {
	// only update agent.LastWork if not recently updated
	if time.Unix(agent.LastWork, 0).Add(updateAgentLastWorkDelay).After(time.Now()) {
		return nil
	}

	agent.LastWork = time.Now().Unix()
	if err := s.store.AgentUpdate(agent); err != nil {
		return err
	}

	return nil
}
