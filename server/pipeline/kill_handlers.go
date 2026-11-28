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
	"fmt"
	"slices"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/queue"
)

// [pts] #435 part 2, a fork divergence from upstream: a workflow that opts in
// with `when: status: killed` still runs after a deliberate cancel, so cleanup
// such as deleting a build VM is not skipped. Upstream evicts every pending
// workflow on cancel and has no `killed` status.

// killHandlers returns the pending workflows of a cancelled pipeline that opted
// in to run after a kill. The opt-in lives on the queue task (RunOn), not on
// the workflow row, so it is read from the queue. A supersede or a cancel of a
// pipeline that never started keeps none: nothing was started to clean up, and
// a superseded push is unchanged by ENG's ruling.
func killHandlers(ctx context.Context, q queue.Queue, workflows []*model.Workflow, cancelInfo *model.CancelInfo, pendingOnly bool) map[int64]bool {
	kept := map[int64]bool{}
	if q == nil || pendingOnly || (cancelInfo != nil && cancelInfo.SupersededBy > 0) {
		return kept
	}
	optedIn := map[string]bool{}
	info := q.Info(ctx)
	for _, task := range slices.Concat(info.Pending, info.WaitingOnDeps) {
		if task != nil && task.RunsOnKilled() {
			optedIn[task.ID] = true
		}
	}
	for _, w := range workflows {
		if w.State == model.StatusPending && optedIn[fmt.Sprint(w.ID)] {
			kept[w.ID] = true
		}
	}
	return kept
}

// neverStarted reports whether no workflow of the pipeline left pending.
func neverStarted(workflows []*model.Workflow) bool {
	for _, w := range workflows {
		if w.State != model.StatusPending {
			return false
		}
	}
	return true
}

// FinalPipelineStatus is PipelineStatus, except that a kill handler's own
// success or skip does not turn a killed pipeline partial. Its failure still
// shows.
func FinalPipelineStatus(pl *model.Pipeline) model.StatusValue {
	if pl.CancelInfo == nil || len(pl.CancelInfo.KillHandlers) == 0 {
		return PipelineStatus(pl.Workflows)
	}
	counted := make([]*model.Workflow, 0, len(pl.Workflows))
	for _, w := range pl.Workflows {
		quiet := w.State == model.StatusSuccess || w.State == model.StatusSkipped
		if quiet && slices.Contains(pl.CancelInfo.KillHandlers, w.ID) {
			continue
		}
		counted = append(counted, w)
	}
	if len(counted) == 0 {
		return PipelineStatus(pl.Workflows)
	}
	return PipelineStatus(counted)
}
