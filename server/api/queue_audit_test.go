// Copyright 2026 Peregrine Technology Systems
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0

package api_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.woodpecker-ci.org/woodpecker/v3/server/api"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	store_mocks "go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
)

func TestGetQueueAudit_DefaultParams(t *testing.T) {
	gin.SetMode(gin.TestMode)
	_store := store_mocks.NewMockStore(t)

	expected := []*model.PriorityAudit{
		{ID: 2, ActorEmail: "u@x.com", TaskID: "t", OldPriority: 0, NewPriority: 5, TS: 1000},
		{ID: 1, ActorEmail: "u@x.com", TaskID: "t", OldPriority: 5, NewPriority: 0, TS: 999},
	}
	_store.On("PriorityAuditList", &model.PriorityAuditListOptions{}).Return(expected, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("store", _store)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/queue/audit", nil)

	api.GetQueueAudit(c)

	require.Equal(t, http.StatusOK, w.Code)
	var got []*model.PriorityAudit
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, expected, got)
}

func TestGetQueueAudit_LimitAndBeforeQueryParams(t *testing.T) {
	gin.SetMode(gin.TestMode)
	_store := store_mocks.NewMockStore(t)

	_store.On("PriorityAuditList", &model.PriorityAuditListOptions{Limit: 25, BeforeID: 100}).
		Return([]*model.PriorityAudit{}, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("store", _store)
	c.Request = httptest.NewRequest(http.MethodGet,
		"/api/queue/audit?"+url.Values{"limit": {"25"}, "before": {"100"}}.Encode(), nil)

	api.GetQueueAudit(c)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestGetQueueAudit_InvalidLimit(t *testing.T) {
	cases := []struct {
		name string
		v    string
	}{
		{"non-numeric", "abc"},
		{"negative", "-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			_store := store_mocks.NewMockStore(t)

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Set("store", _store)
			c.Request = httptest.NewRequest(http.MethodGet,
				"/api/queue/audit?limit="+tc.v, nil)

			api.GetQueueAudit(c)
			assert.Equal(t, http.StatusBadRequest, w.Code)
			_store.AssertNotCalled(t, "PriorityAuditList")
		})
	}
}

func TestGetQueueAudit_InvalidBefore(t *testing.T) {
	gin.SetMode(gin.TestMode)
	_store := store_mocks.NewMockStore(t)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("store", _store)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/queue/audit?before=-5", nil)

	api.GetQueueAudit(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	_store.AssertNotCalled(t, "PriorityAuditList")
}

func TestGetQueueAudit_StoreErrorReturns500(t *testing.T) {
	gin.SetMode(gin.TestMode)
	_store := store_mocks.NewMockStore(t)
	_store.On("PriorityAuditList", &model.PriorityAuditListOptions{}).
		Return(nil, errors.New("disk full"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("store", _store)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/queue/audit", nil)

	api.GetQueueAudit(c)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}
