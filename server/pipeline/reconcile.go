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
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/store"
)

// defaultReconcileMinObservations is the number of consecutive Tick calls a
// pipeline must appear orphaned in before the background reconciler kills it.
// At a 2-minute tick interval this gives a 2-4 minute grace window — long
// enough for a spot agent disconnect (WS close 1006) to reconnect and resume
// without the pipeline being misread as a permanent orphan (#176).
const defaultReconcileMinObservations = 2

// ReconcileOrphanedAtStartup runs the aggressive reconcile once at server boot
// (#891). After a restart any DB-running pipeline is treated as orphaned —
// the in-memory queue dispatch state is gone with the previous process, so a
// "running" row that wasn't terminal-flushed before shutdown will never make
// progress unless we kill it.
func ReconcileOrphanedAtStartup(_store store.Store) int {
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
			log.Info().Msgf("reconcile: killing orphaned pipeline %s#%d (was running, no queue task after restart)",
				repo.FullName, pl.Number)
			if killOrphan(_store, pl) {
				reconciled++
			}
		}
	}

	if reconciled > 0 {
		log.Info().Msgf("reconcile: killed %d orphaned running pipelines after restart", reconciled)
	}
	return reconciled
}

// ReconcileOrphaned is the historical entry point used by tests and any
// caller that wants the aggressive (startup-equivalent) behavior. New
// background-timer callers must use OrphanReconciler.Tick instead, which
// applies a grace period.
func ReconcileOrphaned(_store store.Store) int {
	return ReconcileOrphanedAtStartup(_store)
}

// OrphanReconciler is the long-lived state for the 2-minute background
// reconcile timer (#170). It records the first time each "running" pipeline
// was observed without progress and only kills a pipeline after it has been
// observed orphaned across at least minObservations consecutive ticks. This
// avoids the #176 misfire where a spot-agent disconnect briefly leaves a
// legitimate pipeline taskless before the next dispatch.
//
// The startup reconcile (ReconcileOrphanedAtStartup) does NOT use this — at
// startup the queue dispatch state is provably empty and there is no
// possibility of in-flight redelivery.
type OrphanReconciler struct {
	mu              sync.Mutex
	firstSeen       map[int64]time.Time
	minObservations int
	now             func() time.Time // injected for tests
}

// NewOrphanReconciler builds a reconciler with the default grace window.
func NewOrphanReconciler() *OrphanReconciler {
	return &OrphanReconciler{
		firstSeen:       map[int64]time.Time{},
		minObservations: defaultReconcileMinObservations,
		now:             time.Now,
	}
}

// Tick is called periodically by the background timer. It returns the number
// of pipelines killed on this tick.
//
// Algorithm:
//  1. Walk every active "running" pipeline.
//  2. If first time observed: record in firstSeen, do NOT kill — let the
//     next dispatch loop have a chance to redeliver (handles spot-agent
//     reconnect after WS close 1006).
//  3. If observed before AND still running on this tick: kill.
//  4. After the walk: prune firstSeen entries for pipelines no longer
//     in the orphan set (recovered, or already terminal).
func (r *OrphanReconciler) Tick(_store store.Store) int {
	repos, err := _store.RepoListAll(true, &model.ListOptions{Page: 1, PerPage: 200})
	if err != nil {
		log.Error().Err(err).Msg("reconcile: failed to list repos")
		return 0
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	seenThisTick := make(map[int64]struct{})
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
			seenThisTick[pl.ID] = struct{}{}

			firstSeenAt, observedBefore := r.firstSeen[pl.ID]
			if !observedBefore {
				r.firstSeen[pl.ID] = now
				log.Debug().Msgf("reconcile: observed taskless candidate %s#%d, awaiting grace period",
					repo.FullName, pl.Number)
				continue
			}

			log.Info().
				Dur("observed_for", now.Sub(firstSeenAt)).
				Msgf("reconcile: killing orphaned pipeline %s#%d (running, no queue task across ≥%d ticks)",
					repo.FullName, pl.Number, r.minObservations)
			if killOrphan(_store, pl) {
				reconciled++
				delete(r.firstSeen, pl.ID)
			}
		}
	}

	for id := range r.firstSeen {
		if _, stillOrphan := seenThisTick[id]; !stillOrphan {
			delete(r.firstSeen, id)
		}
	}

	if reconciled > 0 {
		log.Info().Msgf("reconcile: killed %d orphaned running pipelines after grace period", reconciled)
	}
	return reconciled
}

// PendingCount reports how many pipelines are currently in the grace window
// (observed once but not yet eligible for kill). Used by tests; can also be
// surfaced as a metric if needed.
func (r *OrphanReconciler) PendingCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.firstSeen)
}

// killOrphan marks one pipeline + its running workflows as killed. Returns
// true on successful pipeline update; workflow update failures are logged
// but don't reverse the pipeline transition.
func killOrphan(_store store.Store, pl *model.Pipeline) bool {
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
	return true
}
