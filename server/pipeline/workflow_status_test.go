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

	"go.woodpecker-ci.org/woodpecker/v3/rpc"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	store_mocks "go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
)

func TestWorkflowStatus(t *testing.T) {
	tests := []struct {
		s []*model.Step
		e model.StatusValue
	}{
		{
			s: []*model.Step{
				{
					State:   model.StatusFailure,
					Failure: model.FailureIgnore,
				},
				{
					State:   model.StatusSuccess,
					Failure: model.FailureFail,
				},
			},
			e: model.StatusSuccess,
		},
		{
			s: []*model.Step{
				{
					State:   model.StatusSuccess,
					Failure: model.FailureFail,
				},
				{
					State:   model.StatusSuccess,
					Failure: model.FailureIgnore,
				},
			},
			e: model.StatusSuccess,
		},
		{
			s: []*model.Step{
				{
					State:   model.StatusFailure,
					Failure: model.FailureFail,
				},
				{
					State:   model.StatusSuccess,
					Failure: model.FailureFail,
				},
			},
			e: model.StatusFailure,
		},
		{
			s: []*model.Step{
				{
					State:   model.StatusSuccess,
					Failure: model.FailureFail,
				},
				{
					State:   model.StatusPending,
					Failure: model.FailureFail,
				},
			},
			e: model.StatusPending,
		},
		{
			s: []*model.Step{
				{
					State:   model.StatusSuccess,
					Failure: model.FailureFail,
				},
				{
					State:   model.StatusPending,
					Failure: model.FailureIgnore,
				},
			},
			e: model.StatusPending,
		},
		{
			s: []*model.Step{
				{
					State:   model.StatusSuccess,
					Failure: model.FailureIgnore,
				},
				{
					State:   model.StatusPending,
					Failure: model.FailureFail,
				},
			},
			e: model.StatusPending,
		},
		{
			s: []*model.Step{
				{
					State:   model.StatusSuccess,
					Failure: model.FailureIgnore,
				},
				{
					State:   model.StatusPending,
					Failure: model.FailureIgnore,
				},
			},
			e: model.StatusPending,
		},
		{
			s: []*model.Step{
				{
					State:   model.StatusRunning,
					Failure: model.FailureFail,
				},
				{
					State:   model.StatusPending,
					Failure: model.FailureFail,
				},
			},
			e: model.StatusRunning,
		},
		{
			s: []*model.Step{
				{
					State:   model.StatusRunning,
					Failure: model.FailureIgnore,
				},
				{
					State:   model.StatusPending,
					Failure: model.FailureIgnore,
				},
			},
			e: model.StatusRunning,
		},
		{
			s: []*model.Step{
				{
					State:   model.StatusRunning,
					Failure: model.FailureIgnore,
				},
				{
					State:   model.StatusPending,
					Failure: model.FailureFail,
				},
			},
			e: model.StatusRunning,
		},
		{
			s: []*model.Step{
				{
					State:   model.StatusRunning,
					Failure: model.FailureFail,
				},
				{
					State:   model.StatusPending,
					Failure: model.FailureIgnore,
				},
			},
			e: model.StatusRunning,
		},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.e, WorkflowStatus(tt.s))
	}
}

func TestUpdateWorkflowStatusToRunning(t *testing.T) {
	t.Run("should update workflow to running status", func(t *testing.T) {
		workflow := model.Workflow{
			ID:    1,
			State: model.StatusPending,
		}
		state := rpc.WorkflowState{
			Started: 1234567890,
		}

		mockStore := store_mocks.NewMockStore(t)
		mockStore.On("WorkflowUpdate", mock.MatchedBy(func(w *model.Workflow) bool {
			return w.ID == 1 && w.State == model.StatusRunning && w.Started == 1234567890
		})).Return(nil)

		result, err := UpdateWorkflowStatusToRunning(mockStore, workflow, state)

		assert.NoError(t, err)
		assert.Equal(t, model.StatusRunning, result.State)
		assert.Equal(t, int64(1234567890), result.Started)
		mockStore.AssertCalled(t, "WorkflowUpdate", mock.Anything)
	})
}

func TestUpdateWorkflowToStatusSkipped(t *testing.T) {
	t.Run("should update workflow to skipped status", func(t *testing.T) {
		workflow := model.Workflow{
			ID:    2,
			State: model.StatusPending,
		}

		mockStore := store_mocks.NewMockStore(t)
		mockStore.On("WorkflowUpdate", mock.MatchedBy(func(w *model.Workflow) bool {
			return w.ID == 2 && w.State == model.StatusSkipped
		})).Return(nil)

		result, err := UpdateWorkflowToStatusSkipped(mockStore, workflow)

		assert.NoError(t, err)
		assert.Equal(t, model.StatusSkipped, result.State)
		mockStore.AssertCalled(t, "WorkflowUpdate", mock.Anything)
	})
}

func TestUpdateWorkflowStatusToDone(t *testing.T) {
	t.Run("should mark as skipped when not started", func(t *testing.T) {
		workflow := model.Workflow{
			ID:       3,
			State:    model.StatusRunning,
			Children: []*model.Step{},
		}
		state := rpc.WorkflowState{
			Started:  0, // Not started
			Finished: 1234567900,
			Error:    "",
		}

		mockStore := store_mocks.NewMockStore(t)
		mockStore.On("WorkflowUpdate", mock.MatchedBy(func(w *model.Workflow) bool {
			return w.State == model.StatusSkipped && w.Finished == 1234567900
		})).Return(nil)

		result, err := UpdateWorkflowStatusToDone(mockStore, workflow, state)

		assert.NoError(t, err)
		assert.Equal(t, model.StatusSkipped, result.State)
		assert.Equal(t, int64(1234567900), result.Finished)
	})

	t.Run("should mark as failure when error exists", func(t *testing.T) {
		workflow := model.Workflow{
			ID:       5,
			State:    model.StatusRunning,
			Children: []*model.Step{},
		}
		state := rpc.WorkflowState{
			Started:  1234567800,
			Finished: 1234567900,
			Error:    "some error occurred",
		}

		mockStore := store_mocks.NewMockStore(t)
		mockStore.On("WorkflowUpdate", mock.MatchedBy(func(w *model.Workflow) bool {
			return w.State == model.StatusFailure
		})).Return(nil)

		result, err := UpdateWorkflowStatusToDone(mockStore, workflow, state)

		assert.NoError(t, err)
		assert.Equal(t, model.StatusFailure, result.State)
		assert.Equal(t, "some error occurred", result.Error)
	})

	t.Run("should mark as success when all children are successful", func(t *testing.T) {
		successStep := &model.Step{
			ID:    1,
			State: model.StatusSuccess,
		}
		workflow := model.Workflow{
			ID:       6,
			State:    model.StatusRunning,
			Children: []*model.Step{successStep},
		}
		state := rpc.WorkflowState{
			Started:  1234567800,
			Finished: 1234567900,
			Error:    "",
		}

		mockStore := store_mocks.NewMockStore(t)
		mockStore.On("WorkflowUpdate", mock.Anything).Return(nil)

		result, err := UpdateWorkflowStatusToDone(mockStore, workflow, state)

		assert.NoError(t, err)
		assert.Equal(t, int64(1234567900), result.Finished)
	})

	// #230/#232: a spurious cancel (SIGTERM from scale-down / spot preemption)
	// arriving after every step finished must NOT retroactively kill a workflow
	// whose work completed successfully.
	t.Run("should reconcile to success when canceled after all steps succeeded", func(t *testing.T) {
		workflow := model.Workflow{
			ID:    7,
			State: model.StatusRunning,
			Children: []*model.Step{
				{ID: 1, State: model.StatusSuccess},
				{ID: 2, State: model.StatusSuccess},
			},
		}
		state := rpc.WorkflowState{
			Started:  1234567800,
			Finished: 1234567900,
			Error:    "Canceled", // ErrCancel.Error() — stamped by the agent on cancel
			Canceled: true,
		}

		mockStore := store_mocks.NewMockStore(t)
		mockStore.On("WorkflowUpdate", mock.MatchedBy(func(w *model.Workflow) bool {
			return w.State == model.StatusSuccess && w.Error == ""
		})).Return(nil)

		result, err := UpdateWorkflowStatusToDone(mockStore, workflow, state)

		assert.NoError(t, err)
		assert.Equal(t, model.StatusSuccess, result.State)
		assert.Empty(t, result.Error, "spurious Canceled error must be cleared")
	})

	// #230/#232: a genuine mid-flight interruption leaves a killed child step;
	// that workflow must stay killed.
	t.Run("should stay killed when a child step was killed mid-execution", func(t *testing.T) {
		workflow := model.Workflow{
			ID:    8,
			State: model.StatusRunning,
			Children: []*model.Step{
				{ID: 1, State: model.StatusSuccess},
				{ID: 2, State: model.StatusKilled}, // running step, canceled mid-run
				{ID: 3, State: model.StatusSkipped},
			},
		}
		state := rpc.WorkflowState{
			Started:  1234567800,
			Finished: 1234567900,
			Error:    "Canceled",
			Canceled: true,
		}

		mockStore := store_mocks.NewMockStore(t)
		mockStore.On("WorkflowUpdate", mock.MatchedBy(func(w *model.Workflow) bool {
			return w.State == model.StatusKilled
		})).Return(nil)

		result, err := UpdateWorkflowStatusToDone(mockStore, workflow, state)

		assert.NoError(t, err)
		assert.Equal(t, model.StatusKilled, result.State)
	})

	// A canceled workflow with no recorded steps cannot be proven to have
	// completed its work, so it stays killed.
	t.Run("should stay killed when canceled with no children", func(t *testing.T) {
		workflow := model.Workflow{
			ID:       9,
			State:    model.StatusRunning,
			Children: []*model.Step{},
		}
		state := rpc.WorkflowState{
			Started:  1234567800,
			Finished: 1234567900,
			Error:    "Canceled",
			Canceled: true,
		}

		mockStore := store_mocks.NewMockStore(t)
		mockStore.On("WorkflowUpdate", mock.MatchedBy(func(w *model.Workflow) bool {
			return w.State == model.StatusKilled
		})).Return(nil)

		result, err := UpdateWorkflowStatusToDone(mockStore, workflow, state)

		assert.NoError(t, err)
		assert.Equal(t, model.StatusKilled, result.State)
	})

	// A real failure that coincides with a cancel must surface as failure, not
	// be masked as success or killed.
	t.Run("should report failure when canceled but a step failed", func(t *testing.T) {
		workflow := model.Workflow{
			ID:    10,
			State: model.StatusRunning,
			Children: []*model.Step{
				{ID: 1, State: model.StatusSuccess, Failure: model.FailureFail},
				{ID: 2, State: model.StatusFailure, Failure: model.FailureFail},
			},
		}
		state := rpc.WorkflowState{
			Started:  1234567800,
			Finished: 1234567900,
			Error:    "Canceled",
			Canceled: true,
		}

		mockStore := store_mocks.NewMockStore(t)
		mockStore.On("WorkflowUpdate", mock.MatchedBy(func(w *model.Workflow) bool {
			return w.State == model.StatusFailure
		})).Return(nil)

		result, err := UpdateWorkflowStatusToDone(mockStore, workflow, state)

		assert.NoError(t, err)
		assert.Equal(t, model.StatusFailure, result.State)
	})
}
