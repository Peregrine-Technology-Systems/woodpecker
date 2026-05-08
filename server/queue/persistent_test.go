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
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	storemocks "go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
)

func TestWithTaskStore_EvictsZombieTasks(t *testing.T) {
	tasks := []*model.Task{
		{ID: "alive-1", PipelineID: 1},
		{ID: "alive-2", PipelineID: 2},
		{ID: "zombie-killed", PipelineID: 10},
		{ID: "zombie-success", PipelineID: 11},
		{ID: "zombie-error", PipelineID: 12},
		{ID: "zombie-no-pipeline", PipelineID: 99},
	}

	s := storemocks.NewMockStore(t)
	s.On("TaskList").Return(tasks, nil)
	s.On("GetPipeline", int64(1)).Return(&model.Pipeline{ID: 1, Status: model.StatusRunning}, nil)
	s.On("GetPipeline", int64(2)).Return(&model.Pipeline{ID: 2, Status: model.StatusPending}, nil)
	s.On("GetPipeline", int64(10)).Return(&model.Pipeline{ID: 10, Status: model.StatusKilled}, nil)
	s.On("GetPipeline", int64(11)).Return(&model.Pipeline{ID: 11, Status: model.StatusSuccess}, nil)
	s.On("GetPipeline", int64(12)).Return(&model.Pipeline{ID: 12, Status: model.StatusError}, nil)
	s.On("GetPipeline", int64(99)).Return(nil, fmt.Errorf("not found"))
	s.On("TaskDelete", mock.AnythingOfType("string")).Return(nil)

	ctx := context.Background()
	inner, _ := New(ctx, Config{Backend: TypeMemory})
	WithTaskStore(ctx, inner, s)

	// Verify zombie tasks were deleted
	for _, zombie := range []string{"zombie-killed", "zombie-success", "zombie-error", "zombie-no-pipeline"} {
		s.AssertCalled(t, "TaskDelete", zombie)
	}
	// Verify alive tasks were NOT deleted
	s.AssertNotCalled(t, "TaskDelete", "alive-1")
	s.AssertNotCalled(t, "TaskDelete", "alive-2")
}

func TestIsTerminalPipeline(t *testing.T) {
	terminal := []model.StatusValue{
		model.StatusSuccess, model.StatusFailure, model.StatusKilled,
		model.StatusError, model.StatusDeclined, model.StatusSkipped,
		model.StatusSuperseded, model.StatusCanceled,
	}
	active := []model.StatusValue{
		model.StatusRunning, model.StatusPending, model.StatusCreated, model.StatusBlocked,
	}
	for _, s := range terminal {
		assert.True(t, isTerminalPipeline(s), "expected terminal: %s", s)
	}
	for _, s := range active {
		assert.False(t, isTerminalPipeline(s), "expected active: %s", s)
	}
}
