// Copyright 2023 Woodpecker Authors
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
	"go.woodpecker-ci.org/woodpecker/v3/rpc"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/store"
)

// WorkflowStatus determine workflow status based on corresponding step list.
func WorkflowStatus(steps []*model.Step) model.StatusValue {
	status := model.StatusSuccess

	for _, p := range steps {
		if p.Failure == model.FailureFail || !p.Failing() {
			status = MergeStatusValues(status, p.State)
		}
	}

	return status
}

func UpdateWorkflowStatusToRunning(store store.Store, workflow model.Workflow, state rpc.WorkflowState) (*model.Workflow, error) {
	workflow.Started = state.Started
	workflow.State = model.StatusRunning
	return &workflow, store.WorkflowUpdate(&workflow)
}

func UpdateWorkflowToStatusSkipped(store store.Store, workflow model.Workflow) (*model.Workflow, error) {
	workflow.State = model.StatusSkipped
	return &workflow, store.WorkflowUpdate(&workflow)
}

func UpdateWorkflowStatusToDone(store store.Store, workflow model.Workflow, state rpc.WorkflowState) (*model.Workflow, error) {
	workflow.Finished = state.Finished
	workflow.Error = state.Error
	switch {
	case state.Started == 0:
		workflow.State = model.StatusSkipped
	case state.Canceled:
		// #230/#232: a cancel signal — SIGTERM from a scale-down or spot
		// preemption, or a server-side cancel — can arrive AFTER every step has
		// already reached a terminal state. Steps are pre-created as pending at
		// pipeline start, and a running step that is genuinely interrupted is
		// recorded as StatusKilled via its own step update (see
		// step_status.go). So a killed child is the authoritative signal that
		// work was actually cut off. When no child was killed, the cancel is
		// spurious (the agent's workflow context was canceled after the work
		// completed) — honor the steps' real outcome rather than retroactively
		// killing a workflow whose every step succeeded.
		if len(workflow.Children) == 0 || anyStepKilled(workflow.Children) {
			workflow.State = model.StatusKilled
		} else {
			workflow.State = WorkflowStatus(workflow.Children)
			workflow.Error = ""
		}
	case workflow.Error != "":
		workflow.State = model.StatusFailure
	default:
		workflow.State = WorkflowStatus(workflow.Children)
	}
	return &workflow, store.WorkflowUpdate(&workflow)
}

// anyStepKilled reports whether any of a workflow's steps was killed
// mid-execution. A running step that is canceled is recorded as StatusKilled
// via its step update, so a killed child is the authoritative signal that the
// workflow was genuinely interrupted rather than having completed before a late
// cancel arrived. (#230/#232)
func anyStepKilled(steps []*model.Step) bool {
	for _, s := range steps {
		if s.State == model.StatusKilled {
			return true
		}
	}
	return false
}
