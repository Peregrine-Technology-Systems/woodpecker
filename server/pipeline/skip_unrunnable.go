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
	"strconv"
	"time"

	"github.com/rs/zerolog/log"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/queue"
	"go.woodpecker-ci.org/woodpecker/v3/server/store"
)

// SkipUnrunnableWorkflows settles workflows of pl that can never run, and
// returns how many it skipped (#382).
//
// Woodpecker only decides "this task should be skipped" inside the agent poll
// loop, so a workflow pinned to a label no agent advertises is never evaluated.
// The common way to reach that state is a pipeline whose earlier workflow
// PROVISIONS the agent a later one needs: when the earlier one fails, the agent
// never exists, its dependent is never polled, and the pipeline reports
// "running" forever while holding queue entries. The reaping workflow, if there
// is one, is exactly the one that cannot execute.
//
// Each skip is applied in BOTH places that matter: the store (so the pipeline
// can finalize — IsThereRunningStage reads workflow rows) and the queue (so the
// task stops occupying it). Going through queue.Done also cascades the new
// status onto that task's own dependents, so a chain settles across passes
// without this function knowing the graph.
func SkipUnrunnableWorkflows(ctx context.Context, _store store.Store, q queue.Queue, pl *model.Pipeline) int {
	if q == nil || pl == nil {
		return 0
	}
	skipped := 0
	// Bounded by the workflow count: each pass settles at least one workflow or
	// stops. A chain needs one pass per link because a link only becomes
	// evaluable once its predecessor has reported a terminal status.
	for range len(pl.Workflows) + 1 {
		n := skipUnrunnablePass(ctx, _store, q, pl)
		if n == 0 {
			break
		}
		skipped += n
	}
	return skipped
}

// skipUnrunnablePass skips every currently-decidable workflow, once.
func skipUnrunnablePass(ctx context.Context, _store store.Store, q queue.Queue, pl *model.Pipeline) int {
	info := q.Info(ctx)
	byID := workflowsByTaskID(pl)
	skipped := 0

	for _, task := range append(append([]*model.Task{}, info.Pending...), info.WaitingOnDeps...) {
		workflow, mine := byID[task.ID]
		if !mine || !task.CanNeverRun() {
			continue
		}
		if skipWorkflow(ctx, _store, q, workflow, task.ID) {
			skipped++
		}
	}
	return skipped
}

// workflowsByTaskID indexes pl's not-yet-settled workflows by their queue task
// id, so a pass only ever touches workflows of THIS pipeline.
func workflowsByTaskID(pl *model.Pipeline) map[string]*model.Workflow {
	out := map[string]*model.Workflow{}
	for _, workflow := range pl.Workflows {
		if workflow != nil && workflow.Running() {
			out[strconv.FormatInt(workflow.ID, 10)] = workflow
		}
	}
	return out
}

// skipWorkflow persists the skip, then releases the queue task. Store first: a
// queue release whose store write failed would drop the task while leaving the
// pipeline hanging, which is the bug this function exists to fix.
func skipWorkflow(ctx context.Context, _store store.Store, q queue.Queue, workflow *model.Workflow, taskID string) bool {
	workflow.State = model.StatusSkipped
	if workflow.Finished == 0 {
		workflow.Finished = time.Now().Unix()
	}
	if err := _store.WorkflowUpdate(workflow); err != nil {
		log.Error().Err(err).Int64("workflow_id", workflow.ID).
			Msg("skip-unrunnable: cannot mark workflow skipped (#382)")
		return false
	}
	if err := q.Done(ctx, taskID, model.StatusSkipped); err != nil {
		log.Error().Err(err).Str("task_id", taskID).
			Msg("skip-unrunnable: workflow marked skipped but queue release failed (#382)")
	}
	log.Info().Int64("workflow_id", workflow.ID).Str("name", workflow.Name).
		Msg("skip-unrunnable: dependencies settled and workflow can never run, skipped (#382)")
	return true
}
