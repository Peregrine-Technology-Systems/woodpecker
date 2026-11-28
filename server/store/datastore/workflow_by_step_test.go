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

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/store/types"
)

// #418: a step's workflow is the one in the same pipeline whose PID is the
// step's PPID. Two pipelines reuse the same PIDs, so the pipeline must match.
func TestWorkflowByStep(t *testing.T) {
	store, closer := newTestStore(t, new(model.Step), new(model.Pipeline), new(model.Workflow))
	defer closer()

	require.NoError(t, store.WorkflowsCreate([]*model.Workflow{
		{PipelineID: 1, PID: 1, Name: "build"},
		{PipelineID: 1, PID: 2, Name: "test"},
		{PipelineID: 2, PID: 1, Name: "other-pipeline"},
	}))

	wf, err := store.WorkflowByStep(&model.Step{PipelineID: 1, PPID: 2})
	require.NoError(t, err)
	assert.Equal(t, "test", wf.Name)

	wf, err = store.WorkflowByStep(&model.Step{PipelineID: 2, PPID: 1})
	require.NoError(t, err)
	assert.Equal(t, "other-pipeline", wf.Name)

	_, err = store.WorkflowByStep(&model.Step{PipelineID: 1, PPID: 9})
	assert.ErrorIs(t, err, types.ErrRecordNotExist)
}
