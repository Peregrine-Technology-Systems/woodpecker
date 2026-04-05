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

package statusapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/store"
	mocks_store "go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func setupRouter(token string) *gin.Engine {
	r := gin.New()
	h := New(token)
	h.RegisterRoutes(r.Group("/api/plugins/status-api"))
	return r
}

func setupRouterWithStore(token string, _store store.Store) *gin.Engine {
	r := gin.New()
	r.Use(func(c *gin.Context) {
		store.ToContext(c, _store)
		c.Next()
	})
	h := New(token)
	h.RegisterRoutes(r.Group("/api/plugins/status-api"))
	return r
}

func TestName(t *testing.T) {
	h := New("test-token")
	assert.Equal(t, "status-api", h.Name())
}

func TestClose(t *testing.T) {
	h := New("test-token")
	assert.NoError(t, h.Close())
}

func TestBearerAuthValid(t *testing.T) {
	r := setupRouter("secret-token")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/plugins/status-api/workflows/1/init", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer secret-token")
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	// Auth passes but store is nil in test → 500 (not 401)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "store not available")
}

func TestBearerAuthMissing(t *testing.T) {
	r := setupRouter("secret-token")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/plugins/status-api/workflows/1/init", strings.NewReader("{}"))
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestBearerAuthWrongToken(t *testing.T) {
	r := setupRouter("secret-token")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/plugins/status-api/workflows/1/init", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer wrong-token")
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestBearerAuthNotBearer(t *testing.T) {
	r := setupRouter("secret-token")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/plugins/status-api/workflows/1/init", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestInvalidWorkflowID(t *testing.T) {
	mockStore := mocks_store.NewMockStore(t)
	r := setupRouterWithStore("token", mockStore)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/plugins/status-api/workflows/abc/init", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestInitWorkflowNotFound(t *testing.T) {
	mockStore := mocks_store.NewMockStore(t)
	mockStore.On("WorkflowLoad", int64(999)).Return(nil, fmt.Errorf("not found"))
	r := setupRouterWithStore("token", mockStore)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/plugins/status-api/workflows/999/init",
		strings.NewReader(`{"started": 1234567890}`))
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestInitSuccess(t *testing.T) {
	mockStore := mocks_store.NewMockStore(t)
	workflow := &model.Workflow{ID: 1, PipelineID: 10, State: model.StatusPending}
	pl := &model.Pipeline{ID: 10, RepoID: 100, Status: model.StatusRunning}
	repo := &model.Repo{ID: 100, FullName: "org/repo"}

	mockStore.On("WorkflowLoad", int64(1)).Return(workflow, nil)
	mockStore.On("GetPipeline", int64(10)).Return(pl, nil)
	mockStore.On("GetRepo", int64(100)).Return(repo, nil)
	mockStore.On("WorkflowUpdate", mock.AnythingOfType("*model.Workflow")).Return(nil)

	r := setupRouterWithStore("token", mockStore)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/plugins/status-api/workflows/1/init",
		strings.NewReader(`{"started": 1234567890}`))
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpdateStepNotFound(t *testing.T) {
	mockStore := mocks_store.NewMockStore(t)
	workflow := &model.Workflow{ID: 1, PipelineID: 10}
	pl := &model.Pipeline{ID: 10, RepoID: 100, Status: model.StatusRunning}
	repo := &model.Repo{ID: 100, FullName: "org/repo"}

	mockStore.On("WorkflowLoad", int64(1)).Return(workflow, nil)
	mockStore.On("GetPipeline", int64(10)).Return(pl, nil)
	mockStore.On("GetRepo", int64(100)).Return(repo, nil)
	mockStore.On("StepByUUID", "nonexistent").Return(nil, fmt.Errorf("not found"))

	r := setupRouterWithStore("token", mockStore)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/plugins/status-api/workflows/1/update",
		strings.NewReader(`{"step_uuid": "nonexistent", "started": 123}`))
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestRoutesRegistered(t *testing.T) {
	r := setupRouter("token")
	routes := r.Routes()

	paths := make(map[string]bool)
	for _, route := range routes {
		paths[route.Method+" "+route.Path] = true
	}

	assert.True(t, paths["POST /api/plugins/status-api/workflows/:id/init"])
	assert.True(t, paths["POST /api/plugins/status-api/workflows/:id/update"])
	assert.True(t, paths["POST /api/plugins/status-api/workflows/:id/done"])
}
