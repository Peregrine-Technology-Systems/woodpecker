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

package datastore

import "go.woodpecker-ci.org/woodpecker/v3/server/model"

// WorkflowByStep returns the workflow a step belongs to: the one in the same
// pipeline whose PID is the step's PPID (#418, from upstream #6759).
func (s storage) WorkflowByStep(step *model.Step) (*model.Workflow, error) {
	workflow := new(model.Workflow)
	return workflow, wrapGet(s.engine.
		Where("pipeline_id = ?", step.PipelineID).
		Where("pid = ?", step.PPID).
		Get(workflow))
}
