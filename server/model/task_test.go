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

package model

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.woodpecker-ci.org/woodpecker/v3/pipeline"
)

func TestTask_GetLabels(t *testing.T) {
	t.Run("Nil Repo", func(t *testing.T) {
		task := &Task{}
		err := task.ApplyLabelsFromRepo(nil)

		assert.Error(t, err)
		assert.Nil(t, task.Labels)
		assert.EqualError(t, err, "repo is nil but needed to get task labels")
	})

	t.Run("Empty Repo", func(t *testing.T) {
		task := &Task{}
		repo := &Repo{}

		err := task.ApplyLabelsFromRepo(repo)

		assert.NoError(t, err)
		assert.NotNil(t, task.Labels)
		assert.Equal(t, map[string]string{
			pipeline.LabelFilterRepo: "",
			pipeline.LabelFilterOrg:  "0",
		}, task.Labels)
	})

	t.Run("Empty Labels", func(t *testing.T) {
		task := &Task{}
		repo := &Repo{
			FullName: "test/repo",
			ID:       123,
			OrgID:    456,
		}

		err := task.ApplyLabelsFromRepo(repo)

		assert.NoError(t, err)
		assert.NotNil(t, task.Labels)
		assert.Equal(t, map[string]string{
			pipeline.LabelFilterRepo: "test/repo",
			pipeline.LabelFilterOrg:  "456",
		}, task.Labels)
	})

	t.Run("Existing Labels", func(t *testing.T) {
		task := &Task{
			Labels: map[string]string{
				"existing": "label",
			},
		}
		repo := &Repo{
			FullName: "test/repo",
			ID:       123,
			OrgID:    456,
		}

		err := task.ApplyLabelsFromRepo(repo)

		assert.NoError(t, err)
		assert.NotNil(t, task.Labels)
		assert.Equal(t, map[string]string{
			"existing":               "label",
			pipeline.LabelFilterRepo: "test/repo",
			pipeline.LabelFilterOrg:  "456",
		}, task.Labels)
	})
}

func TestTask_TableName(t *testing.T) {
	assert.Equal(t, "tasks", Task{}.TableName())
}

func TestTask_String(t *testing.T) {
	task := &Task{
		ID:           "abc-123",
		Dependencies: []string{"dep1", "dep2"},
		DepStatus:    map[string]StatusValue{"dep1": StatusSuccess},
	}
	got := task.String()
	assert.Contains(t, got, "abc-123")
	assert.Contains(t, got, "dep1")
}

func TestTask_ShouldRun(t *testing.T) {
	t.Run("default RunOn (nil) and all deps successful — should run", func(t *testing.T) {
		task := &Task{
			DepStatus: map[string]StatusValue{
				"dep1": StatusSuccess,
				"dep2": StatusSuccess,
			},
		}
		assert.True(t, task.ShouldRun())
	})

	t.Run("default RunOn (nil) and one dep failed — should NOT run", func(t *testing.T) {
		task := &Task{
			DepStatus: map[string]StatusValue{
				"dep1": StatusSuccess,
				"dep2": StatusFailure,
			},
		}
		assert.False(t, task.ShouldRun())
	})

	t.Run("RunOn=[failure] and one dep failed — should run", func(t *testing.T) {
		task := &Task{
			RunOn:     []string{string(StatusFailure)},
			DepStatus: map[string]StatusValue{"dep1": StatusFailure},
		}
		assert.True(t, task.ShouldRun())
	})

	t.Run("RunOn=[failure] and all deps successful — should NOT run", func(t *testing.T) {
		task := &Task{
			RunOn:     []string{string(StatusFailure)},
			DepStatus: map[string]StatusValue{"dep1": StatusSuccess},
		}
		assert.False(t, task.ShouldRun())
	})

	t.Run("RunOn=[success,failure] — always run", func(t *testing.T) {
		task := &Task{
			RunOn:     []string{string(StatusSuccess), string(StatusFailure)},
			DepStatus: map[string]StatusValue{"dep1": StatusSuccess},
		}
		assert.True(t, task.ShouldRun())
	})

	t.Run("RunOn explicitly empty — same as default", func(t *testing.T) {
		task := &Task{
			RunOn:     []string{},
			DepStatus: map[string]StatusValue{"dep1": StatusSuccess},
		}
		assert.True(t, task.ShouldRun())
	})

	t.Run("RunOn=[other] — neither success nor failure", func(t *testing.T) {
		task := &Task{
			RunOn:     []string{"unknown"},
			DepStatus: map[string]StatusValue{"dep1": StatusSuccess},
		}
		assert.False(t, task.ShouldRun())
	})
}
