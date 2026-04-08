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
	"testing"

	"github.com/stretchr/testify/assert"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
)

func TestFindIndependentWorkflows(t *testing.T) {
	t.Parallel()

	t.Run("identifies workflows with no dependencies", func(t *testing.T) {
		t.Parallel()
		s := mocks.NewMockStore(t)
		s.On("TaskList").Return([]*model.Task{
			{ID: "100", PipelineID: 1, Dependencies: []string{"99"}},
			{ID: "101", PipelineID: 1, Dependencies: []string{}},
			{ID: "102", PipelineID: 1, Dependencies: nil},
		}, nil)

		result := findIndependentWorkflows(s, 1)
		assert.False(t, result[100], "workflow with deps should not be independent")
		assert.True(t, result[101], "workflow with empty deps should be independent")
		assert.True(t, result[102], "workflow with nil deps should be independent")
	})

	t.Run("filters by pipeline ID", func(t *testing.T) {
		t.Parallel()
		s := mocks.NewMockStore(t)
		s.On("TaskList").Return([]*model.Task{
			{ID: "100", PipelineID: 1, Dependencies: nil},
			{ID: "200", PipelineID: 2, Dependencies: nil},
		}, nil)

		result := findIndependentWorkflows(s, 1)
		assert.True(t, result[100])
		assert.False(t, result[200], "task from different pipeline should be excluded")
	})

	t.Run("returns empty map on store error", func(t *testing.T) {
		t.Parallel()
		s := mocks.NewMockStore(t)
		s.On("TaskList").Return(nil, assert.AnError)

		result := findIndependentWorkflows(s, 1)
		assert.Empty(t, result)
	})
}
