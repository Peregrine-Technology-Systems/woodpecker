// Copyright 2026 Peregrine Technology Systems
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0

package api_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"go.woodpecker-ci.org/woodpecker/v3/server"
	"go.woodpecker-ci.org/woodpecker/v3/server/api"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/pubsub"
	"go.woodpecker-ci.org/woodpecker/v3/server/queue"
	queue_mocks "go.woodpecker-ci.org/woodpecker/v3/server/queue/mocks"
	store_mocks "go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
)

// patchTaskPriorityRequest is built into a gin context that mirrors what
// the real router middleware would have set up — store, user, queue,
// pubsub.
func setupPatchPriorityCtx(t *testing.T, body string, headers map[string]string) (*gin.Context, *httptest.ResponseRecorder, *queue_mocks.MockQueue, *store_mocks.MockStore) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	mockStore := store_mocks.NewMockStore(t)
	mockQueue := queue_mocks.NewMockQueue(t)

	server.Config.Services.Pubsub = pubsub.New()
	server.Config.Services.Queue = mockQueue

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("store", mockStore)
	c.Set("user", &model.User{ID: 1, Login: "amal", Email: "amal@pobox.com"})

	req, _ := http.NewRequest(http.MethodPatch, "/", io.NopCloser(bytes.NewBufferString(body)))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	c.Request = req
	c.Params = []gin.Param{{Key: "task_id", Value: "task-abc"}}

	return c, w, mockQueue, mockStore
}

func TestPatchTaskPriority_HappyPath(t *testing.T) {
	c, w, mockQueue, mockStore := setupPatchPriorityCtx(t,
		`{"priority": 5}`, nil)

	mockQueue.On("UpdatePriority", "task-abc", int64(5)).Return(int64(0), nil)
	mockStore.On("PriorityAuditInsert", mock.MatchedBy(func(a *model.PriorityAudit) bool {
		return a.TaskID == "task-abc" &&
			a.OldPriority == 0 &&
			a.NewPriority == 5 &&
			a.ActorEmail == "amal@pobox.com"
	})).Return(nil)

	api.PatchTaskPriority(c)

	require.Equal(t, http.StatusOK, w.Code)
	var got map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, "task_priority_changed", got["type"])
	assert.Equal(t, "task-abc", got["task_id"])
	assert.EqualValues(t, 0, got["old_priority"])
	assert.EqualValues(t, 5, got["new_priority"])
	assert.Equal(t, "amal@pobox.com", got["actor_email"])
}

func TestPatchTaskPriority_OAuth2ProxyHeader(t *testing.T) {
	c, w, mockQueue, mockStore := setupPatchPriorityCtx(t,
		`{"priority": 7}`,
		map[string]string{"X-Auth-Request-Email": "human@pobox.com"})

	mockQueue.On("UpdatePriority", "task-abc", int64(7)).Return(int64(2), nil)
	mockStore.On("PriorityAuditInsert", mock.MatchedBy(func(a *model.PriorityAudit) bool {
		// audit must attribute to the oauth2-proxy header, not the
		// shared admin token's user record
		return a.ActorEmail == "human@pobox.com" && a.OldPriority == 2 && a.NewPriority == 7
	})).Return(nil)

	api.PatchTaskPriority(c)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestPatchTaskPriority_TaskNotPending409(t *testing.T) {
	c, w, mockQueue, _ := setupPatchPriorityCtx(t, `{"priority": 5}`, nil)

	mockQueue.On("UpdatePriority", "task-abc", int64(5)).Return(int64(0), queue.ErrNotFound)

	api.PatchTaskPriority(c)

	assert.Equal(t, http.StatusConflict, w.Code)
}

func TestPatchTaskPriority_NoUser401(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("store", store_mocks.NewMockStore(t))
	// no "user" set
	req, _ := http.NewRequest(http.MethodPatch, "/", io.NopCloser(bytes.NewBufferString(`{"priority": 5}`)))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Params = []gin.Param{{Key: "task_id", Value: "task-abc"}}

	api.PatchTaskPriority(c)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestPatchTaskPriority_BadBody(t *testing.T) {
	c, w, _, _ := setupPatchPriorityCtx(t, `not json`, nil)
	api.PatchTaskPriority(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestPatchTaskPriority_AuditFailureDoesNotFailRequest(t *testing.T) {
	// Per the SOC 2 design: if audit fails after a successful priority
	// change, the request still succeeds (reverting would create a worse
	// state) and the failure is logged for compliance investigation.
	c, w, mockQueue, mockStore := setupPatchPriorityCtx(t,
		`{"priority": 3}`, nil)

	mockQueue.On("UpdatePriority", "task-abc", int64(3)).Return(int64(0), nil)
	mockStore.On("PriorityAuditInsert", mock.Anything).Return(errors.New("disk full"))

	api.PatchTaskPriority(c)
	assert.Equal(t, http.StatusOK, w.Code)
}

// TestPatchTaskPriority_MissingTaskID covers the path where the route
// param is empty (defensive guard in the handler).
func TestPatchTaskPriority_MissingTaskID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("store", store_mocks.NewMockStore(t))
	c.Set("user", &model.User{ID: 1, Login: "amal", Email: "amal@pobox.com"})
	req, _ := http.NewRequest(http.MethodPatch, "/", io.NopCloser(bytes.NewBufferString(`{"priority": 5}`)))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	// note: no Params set, so c.Param("task_id") returns ""

	api.PatchTaskPriority(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestPatchTaskPriority_FallsBackToUserLogin covers the third-tier actor
// attribution: oauth2-proxy header AND user.Email both empty, fall through
// to user.Login so the audit row is never empty.
func TestPatchTaskPriority_FallsBackToUserLogin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockStore := store_mocks.NewMockStore(t)
	mockQueue := queue_mocks.NewMockQueue(t)
	server.Config.Services.Pubsub = pubsub.New()
	server.Config.Services.Queue = mockQueue

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("store", mockStore)
	// User has Login but no Email
	c.Set("user", &model.User{ID: 1, Login: "amal", Email: ""})
	req, _ := http.NewRequest(http.MethodPatch, "/", io.NopCloser(bytes.NewBufferString(`{"priority": 4}`)))
	req.Header.Set("Content-Type", "application/json")
	// no X-Auth-Request-Email header either
	c.Request = req
	c.Params = []gin.Param{{Key: "task_id", Value: "task-fallback"}}

	mockQueue.On("UpdatePriority", "task-fallback", int64(4)).Return(int64(0), nil)
	mockStore.On("PriorityAuditInsert", mock.MatchedBy(func(a *model.PriorityAudit) bool {
		return a.ActorEmail == "amal" // fell back to Login
	})).Return(nil)

	api.PatchTaskPriority(c)
	assert.Equal(t, http.StatusOK, w.Code)
}

// TestPatchTaskPriority_QueueErrorReturns500 covers the path where the
// queue update fails for a reason other than ErrNotFound — must surface
// as 500, not 409.
func TestPatchTaskPriority_QueueErrorReturns500(t *testing.T) {
	c, w, mockQueue, _ := setupPatchPriorityCtx(t, `{"priority": 5}`, nil)

	mockQueue.On("UpdatePriority", "task-abc", int64(5)).
		Return(int64(0), errors.New("unexpected error"))

	api.PatchTaskPriority(c)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}
