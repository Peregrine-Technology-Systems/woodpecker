// Copyright 2024 Woodpecker Authors
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

package statusapi

import (
	"fmt"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/pipeline"
	"go.woodpecker-ci.org/woodpecker/v3/server/plugin"
	"go.woodpecker-ci.org/woodpecker/v3/server/store"
)

// loadWorkflowContext loads the workflow, its pipeline, and repo from the store.
func loadWorkflowContext(_store store.Store, workflowID int64) (*model.Workflow, *model.Pipeline, *model.Repo, error) {
	workflow, err := _store.WorkflowLoad(workflowID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("workflow %d not found: %w", workflowID, err)
	}

	pl, err := _store.GetPipeline(workflow.PipelineID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("pipeline %d not found: %w", workflow.PipelineID, err)
	}

	repo, err := _store.GetRepo(pl.RepoID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("repo %d not found: %w", pl.RepoID, err)
	}

	return workflow, pl, repo, nil
}

// finalizePipelineIfDone checks if all workflows are complete and, if so,
// updates the pipeline status and emits a completion event.
func finalizePipelineIfDone(_store store.Store, pl *model.Pipeline, repo *model.Repo, finished int64) error {
	pl.Workflows, _ = _store.WorkflowGetTree(pl)

	for _, wf := range pl.Workflows {
		if wf.Running() {
			return nil // still running
		}
	}

	status := pipeline.PipelineStatus(pl.Workflows)
	updatedPl, err := pipeline.UpdateStatusToDone(_store, *pl, status, finished)
	if err != nil {
		return err
	}

	pipeline.EmitEvent(plugin.EventPipelineCompleted, repo, updatedPl, "")
	return nil
}
