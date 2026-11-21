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

package pipeline

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog/log"

	"go.woodpecker-ci.org/woodpecker/v3/server"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/queue"
	"go.woodpecker-ci.org/woodpecker/v3/server/store"
)

// Reasons the steady-state reconciler declined to kill a running pipeline that
// its tick-start queue snapshot showed as having no task (#394).
const (
	heldQueueRecheck   = "queue_recheck"   // task found in a fresh queue read
	heldAgentLive      = "agent_live"      // a running workflow is on a live agent
	heldTreeUnreadable = "tree_unreadable" // cannot show the pipeline is unheld
)

// reconcileHeldTotal is the positive signal for #394's acceptance: a busy fleet
// reconciling without killing a live pipeline shows holds here, which tells
// "reconcile ran and declined" apart from "reconcile did not run".
var reconcileHeldTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "woodpecker",
	Name:      "reconcile_held_total",
	Help:      "Running pipelines the orphan reconciler declined to kill, by reason (#394).",
}, []string{"reason"})

// activePipelineSet returns the set of pipeline IDs that have at least one
// task in the in-memory queue (Pending, Running, or WaitingOnDeps). The
// reconciler uses this to skip pipelines that are actively being worked on
// — a heartbeating agent's `Extend()` calls keep its task in q.Running, and
// a disconnected agent's task is moved by `resubmitExpiredPipelines` from
// running back to pending (still in the queue), so the queue is the single
// source of truth for "is this pipeline making progress, or about to."
//
// If the queue is nil (test contexts), returns an empty set — every
// running pipeline is treated as orphaned, matching the legacy aggressive
// behavior.
func activePipelineSet(q queue.Queue) map[int64]struct{} {
	set := make(map[int64]struct{})
	if q == nil {
		return set
	}
	info := q.Info(context.Background())
	add := func(tasks []*model.Task) {
		for _, t := range tasks {
			if t != nil && t.PipelineID != 0 {
				set[t.PipelineID] = struct{}{}
			}
		}
	}
	add(info.Pending)
	add(info.Running)
	add(info.WaitingOnDeps)
	return set
}

// ReconcileOrphanedAtStartup runs once at server boot (#891). After a
// restart any DB-running pipeline whose tasks weren't restored to the
// queue is genuinely orphaned — the in-memory dispatch state is gone and
// the only way the row will ever transition is if we kill it.
//
// At startup the queue HAS been populated by `WithTaskStore` from the
// persistent task store (per `server/queue/persistent.go`), including the
// work agents were running when the server stopped, which goes back into
// q.running on its agent (#400). Before #400 the store row was deleted on
// Poll, so every running workflow was missing here and every running pipeline
// with nothing left to dispatch was killed on each restart. Now a row running
// with no queue task is provably lost, and killing immediately is safe.
func ReconcileOrphanedAtStartup(_store store.Store, q queue.Queue) int {
	active := activePipelineSet(q)
	repos, err := _store.RepoListAll(true, &model.ListOptions{Page: 1, PerPage: 200})
	if err != nil {
		log.Error().Err(err).Msg("reconcile: failed to list repos")
		return 0
	}

	reconciled := 0
	for _, repo := range repos {
		pipelines, err := _store.GetActivePipelineList(repo)
		if err != nil {
			continue
		}
		for _, pl := range pipelines {
			if pl.Status != model.StatusRunning {
				continue
			}
			if _, hasTask := active[pl.ID]; hasTask {
				log.Debug().Msgf("reconcile: %s#%d has active queue task — skipping (post-restart restore)",
					repo.FullName, pl.Number)
				continue
			}
			log.Info().Msgf("reconcile: killing orphaned pipeline %s#%d (was running, no queue task after restart)",
				repo.FullName, pl.Number)
			if killOrphan(context.Background(), _store, pl, repo) {
				reconciled++
			}
		}
	}

	if reconciled > 0 {
		log.Info().Msgf("reconcile: killed %d orphaned running pipelines after restart", reconciled)
	}
	return reconciled
}

// ReconcileOrphaned is the historical entry point. Kept for back-compat
// with callers that don't want to wire up the queue dependency — but the
// queue-less form treats every running pipeline as a kill candidate, which
// is only correct at startup. Callers in steady-state code MUST use
// OrphanReconciler.Tick or pass the queue explicitly to
// ReconcileOrphanedAtStartup.
func ReconcileOrphaned(_store store.Store) int {
	return ReconcileOrphanedAtStartup(_store, nil)
}

// OrphanReconciler holds the queue handle for the steady-state timer.
// On every Tick it asks the queue which pipelines have active tasks and
// only kills the rest. The earlier "grace period across consecutive ticks"
// (#176) was a workaround for the same symptom — the queue check is the
// principled replacement.
type OrphanReconciler struct {
	queue     queue.Queue
	agentLive func(agentID int64) bool
}

// SetAgentLiveness injects the oracle that says whether an agent is still live
// (#394). Wired in cmd/server/setup.go to a transport-agnostic LastContact
// check, so it covers gRPC agents and survives a server restart, which the
// in-memory WS connection sets do not. Nil leaves the queue as the only signal.
func (r *OrphanReconciler) SetAgentLiveness(fn func(agentID int64) bool) {
	r.agentLive = fn
}

// holdReason reports why a running pipeline that the tick-start snapshot shows
// without a queue task must not be killed, or "" if it is orphaned (#394).
//
// The snapshot is taken once, before a walk over every repo, and a pipeline is
// only marked running by rpc.Init, which follows Poll moving its task into
// q.running. So a pipeline read as running after the snapshot was taken can
// have its task in the queue without the snapshot showing it: infra#18796 was
// started at 14:20:42Z and killed at 14:20:43Z. A fresh queue read, taken after
// the store read, is ordered correctly and is the root fix.
//
// The live-agent check is the backstop: whatever removed the task, a pipeline
// with a running workflow on a live agent is not orphaned. A kill cannot be
// undone, so a tree that cannot be read holds until the next tick.
func (r *OrphanReconciler) holdReason(_store store.Store, pl *model.Pipeline) string {
	if _, ok := activePipelineSet(r.queue)[pl.ID]; ok {
		return heldQueueRecheck
	}
	if r.agentLive == nil {
		return ""
	}
	workflows, err := _store.WorkflowGetTree(pl)
	if err != nil {
		log.Warn().Err(err).Int64("pipeline_id", pl.ID).
			Msg("reconcile: cannot read workflows — holding until next tick")
		return heldTreeUnreadable
	}
	for _, w := range workflows {
		if w.State == model.StatusRunning && w.AgentID > 0 && r.agentLive(w.AgentID) {
			log.Info().Int64("pipeline_id", pl.ID).Int64("workflow_id", w.ID).
				Int64("agent_id", w.AgentID).
				Msg("reconcile: holding pipeline with no queue task — its workflow is on a live agent")
			return heldAgentLive
		}
	}
	return ""
}

// NewOrphanReconciler builds a queue-aware reconciler. The queue MUST be
// the same instance used by the dispatcher; otherwise the active-task
// snapshot won't reflect agent heartbeats.
func NewOrphanReconciler(q queue.Queue) *OrphanReconciler {
	return &OrphanReconciler{queue: q}
}

// Tick walks every running pipeline and kills those without a backing
// queue task. Heartbeating agents keep their task in `q.Running` (via
// Extend); a disconnected agent's task is moved to `q.Pending` by
// resubmitExpiredPipelines (still in the queue). Either way, the queue
// is the source of truth for "is this pipeline making progress."
func (r *OrphanReconciler) Tick(_store store.Store) int {
	active := activePipelineSet(r.queue)

	repos, err := _store.RepoListAll(true, &model.ListOptions{Page: 1, PerPage: 200})
	if err != nil {
		log.Error().Err(err).Msg("reconcile: failed to list repos")
		return 0
	}

	reconciled := 0
	for _, repo := range repos {
		pipelines, err := _store.GetActivePipelineList(repo)
		if err != nil {
			continue
		}
		for _, pl := range pipelines {
			if pl.Status != model.StatusRunning {
				continue
			}
			if _, hasTask := active[pl.ID]; hasTask {
				log.Debug().Msgf("reconcile: %s#%d has active queue task — skipping",
					repo.FullName, pl.Number)
				continue
			}
			if reason := r.holdReason(_store, pl); reason != "" {
				reconcileHeldTotal.WithLabelValues(reason).Inc()
				log.Info().Str("reason", reason).Msgf("reconcile: not killing %s#%d (running, no queue task in tick snapshot)",
					repo.FullName, pl.Number)
				continue
			}
			log.Info().Msgf("reconcile: killing orphaned pipeline %s#%d (running, no queue task)",
				repo.FullName, pl.Number)
			if killOrphan(context.Background(), _store, pl, repo) {
				reconciled++
			}
		}
	}

	if reconciled > 0 {
		log.Info().Msgf("reconcile: killed %d orphaned running pipelines", reconciled)
	}
	return reconciled
}

// reconcileErrSignature is the workflow.Error / step.Error recorded by
// killOrphan. Deliberately NOT the "agent disconnected" string
// Workflow.KilledByAgentDisconnect() matches: that match suppresses the
// forge.Status post (fork#44) on the assumption a requeue will follow up
// with a fresh one shortly. This backstop path has no requeue counterpart
// (woodpecker#349) — suppressing here would leave the GitHub status stuck
// at whatever it last was, forever, with nothing left to ever resolve it.
const reconcileErrSignature = "reconcile: pipeline orphaned, no active queue task"

// killOrphan marks one pipeline + every non-terminal workflow underneath it
// as killed — including workflows that never started, not just ones caught
// mid-run (#349) — finalizing their steps via FinalizeKilledWorkflow, and
// posts a final failure status to the forge so the upstream PR check
// transitions out of "pending" (#181, #349). Returns true on successful
// pipeline update; workflow update failures are logged but don't reverse
// the pipeline transition; forge.Status failures are also logged but never
// fail the kill — the DB transition is the durable contract.
func killOrphan(ctx context.Context, _store store.Store, pl *model.Pipeline, repo *model.Repo) bool {
	// #193: structured log mirrors cancel.go::Cancel — same fields so a single
	// query catches every kill/cancel decision regardless of source.
	log.Info().
		Int64("pipeline_id", pl.ID).
		Str("repo", repo.FullName).
		Str("prior_status", string(pl.Status)).
		Str("new_status", string(model.StatusKilled)).
		Bool("never_dispatched", pl.Started == 0).
		Str("reason", "reconcile_orphaned").
		Msg("pipeline cancel: transitioning")
	pl.Status = model.StatusKilled
	pl.KillReason = "reconcile_orphaned"
	pl.KilledAt = time.Now().Unix()
	if err := _store.UpdatePipeline(pl); err != nil {
		log.Error().Err(err).Msgf("reconcile: failed to update pipeline %d", pl.ID)
		return false
	}

	workflows, err := _store.WorkflowGetTree(pl)
	if err != nil {
		return true
	}
	now := time.Now().Unix()
	for _, w := range workflows {
		// Running() covers both Pending (never started) and Running — #349:
		// a not-yet-started workflow left untouched here stays Pending
		// forever, and so does its forge status.
		if w.Running() {
			FinalizeKilledWorkflow(ctx, _store, w, now, reconcileErrSignature)
		}
	}

	pl.Workflows = workflows
	postReconcileForgeStatus(ctx, _store, pl, repo)
	return true
}

// postReconcileForgeStatus is a fail-open wrapper around updatePipelineStatus
// for the reconcile path. Looks up the user and forge service; on any
// missing dependency (typically tests where server.Config.Services isn't
// wired) it logs and returns without failing the caller.
func postReconcileForgeStatus(ctx context.Context, _store store.Store, pl *model.Pipeline, repo *model.Repo) {
	if server.Config.Services.Manager == nil {
		log.Debug().Msgf("reconcile: forge status skip — services manager not configured (likely test context)")
		return
	}
	user, err := _store.GetUser(repo.UserID)
	if err != nil {
		log.Warn().Err(err).Msgf("reconcile: forge status skip — cannot find user %d for repo %s", repo.UserID, repo.FullName)
		return
	}
	_forge, err := server.Config.Services.Manager.ForgeFromRepo(repo)
	if err != nil {
		log.Warn().Err(err).Msgf("reconcile: forge status skip — cannot resolve forge for repo %s", repo.FullName)
		return
	}
	updatePipelineStatus(ctx, _forge, pl, repo, user)
}
