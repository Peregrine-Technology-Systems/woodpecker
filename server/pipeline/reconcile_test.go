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
	"github.com/stretchr/testify/mock"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
)

func TestReconcileOrphaned_KillsRunningPipelines(t *testing.T) {
	mockStore := mocks.NewMockStore(t)

	repos := []*model.Repo{
		{ID: 1, FullName: "org/scanner"},
	}
	mockStore.On("RepoListAll", true, mock.Anything).Return(repos, nil)

	// Scanner has a "running" pipeline orphaned from before restart
	pipelines := []*model.Pipeline{
		{ID: 100, Number: 1020, Status: model.StatusRunning, RepoID: 1},
	}
	mockStore.On("GetActivePipelineList", repos[0]).Return(pipelines, nil)

	// Pipeline should be updated to killed
	mockStore.On("UpdatePipeline", mock.MatchedBy(func(p *model.Pipeline) bool {
		return p.ID == 100 && p.Status == model.StatusKilled
	})).Return(nil)

	// Workflow tree for the pipeline
	workflows := []*model.Workflow{
		{ID: 50, State: model.StatusRunning, Name: "ci"},
	}
	mockStore.On("WorkflowGetTree", pipelines[0]).Return(workflows, nil)

	// Workflow should be updated to killed
	mockStore.On("WorkflowUpdate", mock.MatchedBy(func(w *model.Workflow) bool {
		return w.ID == 50 && w.State == model.StatusKilled
	})).Return(nil)

	count := ReconcileOrphaned(mockStore)
	assert.Equal(t, 1, count)
	mockStore.AssertCalled(t, "UpdatePipeline", mock.Anything)
	mockStore.AssertCalled(t, "WorkflowUpdate", mock.Anything)
}

func TestReconcileOrphaned_SkipsPendingPipelines(t *testing.T) {
	mockStore := mocks.NewMockStore(t)

	repos := []*model.Repo{
		{ID: 1, FullName: "org/repo"},
	}
	mockStore.On("RepoListAll", true, mock.Anything).Return(repos, nil)

	// Only pending pipeline — should NOT be killed (queue will handle it)
	pipelines := []*model.Pipeline{
		{ID: 200, Number: 42, Status: model.StatusPending, RepoID: 1},
	}
	mockStore.On("GetActivePipelineList", repos[0]).Return(pipelines, nil)

	count := ReconcileOrphaned(mockStore)
	assert.Equal(t, 0, count)
	mockStore.AssertNotCalled(t, "UpdatePipeline", mock.Anything)
}

func TestReconcileOrphaned_NoActiveRepos(t *testing.T) {
	mockStore := mocks.NewMockStore(t)

	mockStore.On("RepoListAll", true, mock.Anything).Return([]*model.Repo{}, nil)

	count := ReconcileOrphaned(mockStore)
	assert.Equal(t, 0, count)
}
