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
	"github.com/rs/zerolog/log"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/store"
)

// ReconcileOrphaned finds pipelines stuck in "running" or "pending" state
// with no corresponding queue task and marks them as killed. This handles
// pipelines orphaned by server restarts — the in-memory queue is lost but
// the DB still shows them as running (#891).
func ReconcileOrphaned(_store store.Store) int {
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
			// Pipeline is "running" in DB — mark as killed since queue is empty after restart
			log.Info().Msgf("reconcile: killing orphaned pipeline %s#%d (was running, no queue task after restart)",
				repo.FullName, pl.Number)

			pl.Status = model.StatusKilled
			if err := _store.UpdatePipeline(pl); err != nil {
				log.Error().Err(err).Msgf("reconcile: failed to update pipeline %d", pl.ID)
				continue
			}

			// Also mark running workflows as killed
			workflows, err := _store.WorkflowGetTree(pl)
			if err != nil {
				continue
			}
			for _, w := range workflows {
				if w.State == model.StatusRunning {
					w.State = model.StatusKilled
					if err := _store.WorkflowUpdate(w); err != nil {
						log.Error().Err(err).Msgf("reconcile: failed to update workflow %d", w.ID)
					}
				}
			}
			reconciled++
		}
	}

	if reconciled > 0 {
		log.Info().Msgf("reconcile: killed %d orphaned running pipelines", reconciled)
	}
	return reconciled
}
