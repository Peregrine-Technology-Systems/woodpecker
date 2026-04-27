// Copyright 2026 Woodpecker Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0

package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	forge_mocks "go.woodpecker-ci.org/woodpecker/v3/server/forge/mocks"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
)

// TestUpdatePipelineStatus_BoundedTimeout asserts that updatePipelineStatus
// returns within forgeStatusTimeout × len(workflows) even if forge.Status
// hangs indefinitely. Regression for #30 — slow forge must not block the
// pipeline state machine.
func TestUpdatePipelineStatus_BoundedTimeout(t *testing.T) {
	mockForge := forge_mocks.NewMockForge(t)
	mockForge.On("Status", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			ctx := args.Get(0).(context.Context)
			<-ctx.Done() // hang until the per-call ctx times out
		}).
		Return(context.DeadlineExceeded).
		Once()

	pipeline := &model.Pipeline{
		ID:     1,
		Number: 1,
		Workflows: []*model.Workflow{
			{ID: 10, Name: "build", State: model.StatusPending},
		},
	}
	repo := &model.Repo{FullName: "example/test"}
	user := &model.User{Login: "tester"}

	start := time.Now()
	updatePipelineStatus(context.Background(), mockForge, pipeline, repo, user)
	elapsed := time.Since(start)

	// Worst case is forgeStatusTimeout. Allow a small slack for scheduler
	// overhead but assert we don't hang past 2× the configured timeout.
	assert.Less(t, elapsed, 2*forgeStatusTimeout,
		"updatePipelineStatus blocked for %s (expected <%s)", elapsed, 2*forgeStatusTimeout)
	assert.GreaterOrEqual(t, elapsed, forgeStatusTimeout-100*time.Millisecond,
		"updatePipelineStatus returned too fast (%s) — timeout may not be applied", elapsed)
}

// TestUpdatePipelineStatus_SuccessFastPath asserts a fast-returning forge.Status
// call doesn't pay the full timeout and propagates through all workflows.
func TestUpdatePipelineStatus_SuccessFastPath(t *testing.T) {
	mockForge := forge_mocks.NewMockForge(t)
	mockForge.On("Status", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil).Twice()

	pipeline := &model.Pipeline{
		ID:     2,
		Number: 2,
		Workflows: []*model.Workflow{
			{ID: 20, Name: "test", State: model.StatusPending},
			{ID: 21, Name: "deploy", State: model.StatusPending},
		},
	}
	repo := &model.Repo{FullName: "example/fast"}
	user := &model.User{Login: "tester"}

	start := time.Now()
	updatePipelineStatus(context.Background(), mockForge, pipeline, repo, user)
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 100*time.Millisecond,
		"fast-path took %s (forge.Status mocked to return nil immediately)", elapsed)
	mockForge.AssertExpectations(t)
}
