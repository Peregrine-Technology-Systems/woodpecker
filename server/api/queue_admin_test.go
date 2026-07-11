// Copyright 2026 Woodpecker Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"go.woodpecker-ci.org/woodpecker/v3/server"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/queue"
	queue_mocks "go.woodpecker-ci.org/woodpecker/v3/server/queue/mocks"
	store_mocks "go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
)

// =============================================================================
// getAgentName / processQueueTasks — pure helpers
// =============================================================================

func TestGetAgentName_CacheHit(t *testing.T) {
	cache := map[int64]string{42: "agent-foo"}
	name, ok := getAgentName(nil, cache, 42)
	assert.True(t, ok)
	assert.Equal(t, "agent-foo", name)
}

func TestGetAgentName_StoreMissReturnsFalse(t *testing.T) {
	mockStore := store_mocks.NewMockStore(t)
	mockStore.On("AgentFind", int64(99)).Return(nil, assert.AnError)
	cache := map[int64]string{}
	name, ok := getAgentName(mockStore, cache, 99)
	assert.False(t, ok)
	assert.Equal(t, "", name)
}

func TestGetAgentName_StoreNilAgent(t *testing.T) {
	mockStore := store_mocks.NewMockStore(t)
	mockStore.On("AgentFind", int64(7)).Return(nil, nil)
	cache := map[int64]string{}
	_, ok := getAgentName(mockStore, cache, 7)
	assert.False(t, ok)
}

func TestGetAgentName_PopulatesCache(t *testing.T) {
	mockStore := store_mocks.NewMockStore(t)
	mockStore.On("AgentFind", int64(11)).Return(&model.Agent{ID: 11, Name: "alpha"}, nil).Once()
	cache := map[int64]string{}
	name, ok := getAgentName(mockStore, cache, 11)
	assert.True(t, ok)
	assert.Equal(t, "alpha", name)
	assert.Equal(t, "alpha", cache[11], "second call must hit cache without store")
	// second call: store mock asserts only Once() so a second AgentFind would fail
	name2, ok2 := getAgentName(mockStore, cache, 11)
	assert.True(t, ok2)
	assert.Equal(t, "alpha", name2)
}

func TestGetAgentName_EmptyAgentNameTreatedMissing(t *testing.T) {
	mockStore := store_mocks.NewMockStore(t)
	mockStore.On("AgentFind", int64(12)).Return(&model.Agent{ID: 12, Name: ""}, nil)
	_, ok := getAgentName(mockStore, map[int64]string{}, 12)
	assert.False(t, ok, "agent without name treated as missing")
}

func TestProcessQueueTasks_ZeroAgentZeroPipeline(t *testing.T) {
	mockStore := store_mocks.NewMockStore(t)
	tasks := []*model.Task{{ID: "t1", AgentID: 0, PipelineID: 0}}
	out, err := processQueueTasks(mockStore, tasks, map[int64]string{})
	assert.NoError(t, err)
	assert.Len(t, out, 1)
	assert.Equal(t, "", out[0].AgentName)
}

func TestProcessQueueTasks_UnresolvedAgentUsesHonestIDPlaceholder(t *testing.T) {
	// woodpecker#311: name resolution failing must NOT fabricate a
	// "(disconnected)" status the handler never verified — that lie fed
	// ci-scaler#1723's false orphan-kills. The honest label is the bare id.
	mockStore := store_mocks.NewMockStore(t)
	mockStore.On("AgentFind", int64(50)).Return(nil, assert.AnError)
	tasks := []*model.Task{{ID: "t2", AgentID: 50, PipelineID: 0}}
	out, err := processQueueTasks(mockStore, tasks, map[int64]string{})
	assert.NoError(t, err)
	assert.Equal(t, "agent-50", out[0].AgentName)
	assert.NotContains(t, out[0].AgentName, "disconnected",
		"must not assert a connection state name resolution never checked")
}

func TestProcessQueueTasks_AgentResolved(t *testing.T) {
	mockStore := store_mocks.NewMockStore(t)
	mockStore.On("AgentFind", int64(51)).Return(&model.Agent{ID: 51, Name: "beta"}, nil)
	tasks := []*model.Task{{ID: "t3", AgentID: 51, PipelineID: 0}}
	out, err := processQueueTasks(mockStore, tasks, map[int64]string{})
	assert.NoError(t, err)
	assert.Equal(t, "beta", out[0].AgentName)
}

func TestProcessQueueTasks_PipelineLookupSetsNumber(t *testing.T) {
	mockStore := store_mocks.NewMockStore(t)
	mockStore.On("GetPipeline", int64(900)).Return(&model.Pipeline{ID: 900, Number: 42}, nil)
	tasks := []*model.Task{{ID: "t4", AgentID: 0, PipelineID: 900}}
	out, err := processQueueTasks(mockStore, tasks, map[int64]string{})
	assert.NoError(t, err)
	assert.Equal(t, int64(42), out[0].PipelineNumber)
}

func TestProcessQueueTasks_PipelineLookupErrorBubbles(t *testing.T) {
	mockStore := store_mocks.NewMockStore(t)
	mockStore.On("GetPipeline", int64(901)).Return(nil, assert.AnError)
	tasks := []*model.Task{{ID: "t5", AgentID: 0, PipelineID: 901}}
	_, err := processQueueTasks(mockStore, tasks, map[int64]string{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "pipeline not found")
}

// =============================================================================
// Queue admin handlers
// =============================================================================

func setupQueueAdminTest(t *testing.T) (*store_mocks.MockStore, *queue_mocks.MockQueue, *gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	mockStore := store_mocks.NewMockStore(t)
	mockQueue := queue_mocks.NewMockQueue(t)
	server.Config.Services.Queue = mockQueue

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("store", mockStore)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	return mockStore, mockQueue, c, w
}

func TestGetQueueInfo_Success(t *testing.T) {
	mockStore, mockQueue, c, w := setupQueueAdminTest(t)
	info := queue.InfoT{
		Pending:       []*model.Task{{ID: "p1", AgentID: 0, PipelineID: 0}},
		WaitingOnDeps: []*model.Task{{ID: "w1", AgentID: 0, PipelineID: 0}},
		Running:       []*model.Task{{ID: "r1", AgentID: 0, PipelineID: 0}},
		Paused:        false,
	}
	info.Stats.Workers = 2
	info.Stats.Pending = 1
	info.Stats.WaitingOnDeps = 1
	info.Stats.Running = 1
	mockQueue.On("Info", mock.Anything).Return(info)

	GetQueueInfo(c)

	assert.Equal(t, http.StatusOK, w.Code)
	_ = mockStore // store wasn't needed (no agent/pipeline lookups for these tasks)
}

func TestGetQueueInfo_PendingErrorBubbles(t *testing.T) {
	mockStore, mockQueue, c, w := setupQueueAdminTest(t)
	mockQueue.On("Info", mock.Anything).Return(queue.InfoT{
		Pending: []*model.Task{{ID: "p1", PipelineID: 999}}, // forces GetPipeline call
	})
	mockStore.On("GetPipeline", int64(999)).Return(nil, assert.AnError)

	GetQueueInfo(c)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestGetQueueInfo_WaitingErrorBubbles(t *testing.T) {
	mockStore, mockQueue, c, w := setupQueueAdminTest(t)
	mockQueue.On("Info", mock.Anything).Return(queue.InfoT{
		WaitingOnDeps: []*model.Task{{ID: "w1", PipelineID: 998}},
	})
	mockStore.On("GetPipeline", int64(998)).Return(nil, assert.AnError)
	GetQueueInfo(c)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestGetQueueInfo_RunningErrorBubbles(t *testing.T) {
	mockStore, mockQueue, c, w := setupQueueAdminTest(t)
	mockQueue.On("Info", mock.Anything).Return(queue.InfoT{
		Running: []*model.Task{{ID: "r1", PipelineID: 997}},
	})
	mockStore.On("GetPipeline", int64(997)).Return(nil, assert.AnError)
	GetQueueInfo(c)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestPauseQueue(t *testing.T) {
	_, mockQueue, c, _ := setupQueueAdminTest(t)
	mockQueue.On("Pause").Once()
	PauseQueue(c)
	assert.Equal(t, http.StatusNoContent, c.Writer.Status())
}

func TestResumeQueue(t *testing.T) {
	_, mockQueue, c, _ := setupQueueAdminTest(t)
	mockQueue.On("Resume").Once()
	ResumeQueue(c)
	assert.Equal(t, http.StatusNoContent, c.Writer.Status())
}

func TestBlockTilQueueHasRunningItem_ReturnsWhenNoneRunning(t *testing.T) {
	_, mockQueue, c, _ := setupQueueAdminTest(t)
	info := queue.InfoT{}
	info.Stats.Running = 0
	mockQueue.On("Info", mock.Anything).Return(info)
	BlockTilQueueHasRunningItem(c)
	assert.Equal(t, http.StatusNoContent, c.Writer.Status())
}
