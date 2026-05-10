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

	"github.com/rs/zerolog/log"

	"go.woodpecker-ci.org/woodpecker/v3/server"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/queue"
	"go.woodpecker-ci.org/woodpecker/v3/server/store"
)

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
// persistent task store (per `server/queue/persistent.go`), so a row
// running with no queue task is provably lost. Killing immediately is
// safe.
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
	queue queue.Queue
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

// killOrphan marks one pipeline + its running workflows as killed AND
// posts a final failure status to the forge so the upstream PR check
// transitions out of "pending" (#181). Returns true on successful pipeline
// update; workflow update failures are logged but don't reverse the
// pipeline transition; forge.Status failures are also logged but never
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
	if err := _store.UpdatePipeline(pl); err != nil {
		log.Error().Err(err).Msgf("reconcile: failed to update pipeline %d", pl.ID)
		return false
	}

	workflows, err := _store.WorkflowGetTree(pl)
	if err != nil {
		return true
	}
	for _, w := range workflows {
		if w.State == model.StatusRunning {
			w.State = model.StatusKilled
			if err := _store.WorkflowUpdate(w); err != nil {
				log.Error().Err(err).Msgf("reconcile: failed to update workflow %d", w.ID)
			}
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
