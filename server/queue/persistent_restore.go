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

package queue

import (
	"strconv"

	"github.com/rs/zerolog/log"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/store"
)

// runningAdopter puts restored tasks straight into q.running (#400).
type runningAdopter interface {
	adoptRunning(tasks []*model.Task)
}

// restorePlan sorts the stored tasks of a restarting server (#400).
//
// A task row now lives from push until Done/Error, so the store holds the work
// agents were running when the server stopped, not only the work still waiting.
// Work an agent holds (started, or only claimed) goes back to q.running on that
// agent, so its lease, its dependents and the reconcilers all see it; an
// already-finished workflow (its Done was lost in the restart) is evicted and
// its outcome handed to its dependents; anything else waits, as before.
type restorePlan struct {
	pending  []*model.Task
	running  []*model.Task
	evict    []string
	finished map[string]model.StatusValue
}

func planRestore(s store.Store, tasks []*model.Task) restorePlan {
	plan := restorePlan{finished: map[string]model.StatusValue{}}
	for _, task := range tasks {
		pl, err := s.GetPipeline(task.PipelineID)
		if err != nil || isTerminalPipeline(pl.Status) {
			plan.evict = append(plan.evict, task.ID)
			continue
		}
		wf := loadWorkflow(s, task.ID)
		if wf != nil && isTerminalPipeline(wf.State) {
			plan.evict = append(plan.evict, task.ID)
			plan.finished[task.ID] = wf.State
			continue
		}
		if holder := holderOf(task, wf); holder > 0 {
			task.AgentID = holder
			plan.running = append(plan.running, task)
			continue
		}
		plan.pending = append(plan.pending, task)
	}
	for _, task := range plan.pending {
		for _, dep := range task.Dependencies {
			if state, ok := plan.finished[dep]; ok {
				if task.DepStatus == nil {
					task.DepStatus = map[string]model.StatusValue{}
				}
				task.DepStatus[dep] = state
			}
		}
	}
	return plan
}

// holderOf is the agent holding a task, or 0. The workflow row names it once
// the agent has started the work (rpc.Init); the task row names it from the
// moment the agent claimed it (Poll), which the workflow row does not show yet.
func holderOf(task *model.Task, wf *model.Workflow) int64 {
	if wf != nil && wf.State == model.StatusRunning && wf.AgentID > 0 {
		return wf.AgentID
	}
	return task.AgentID
}

func loadWorkflow(s store.Store, taskID string) *model.Workflow {
	id, err := strconv.ParseInt(taskID, 10, 64)
	if err != nil {
		return nil
	}
	wf, err := s.WorkflowLoad(id)
	if err != nil {
		log.Warn().Err(err).Str("task", taskID).Msg("queue restore: cannot read workflow — deciding from the task row alone")
		return nil
	}
	return wf
}
