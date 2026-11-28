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

package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"go.woodpecker-ci.org/woodpecker/v3/server"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/queue"
	queue_mocks "go.woodpecker-ci.org/woodpecker/v3/server/queue/mocks"
	store_mocks "go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
	"go.woodpecker-ci.org/woodpecker/v3/server/store/types"
)

// Every path of the agent handlers in agent.go, which #440 touched. Each case
// sets up the store and queue it needs and checks the status the caller sees.

type agentCase struct {
	name    string
	handler gin.HandlerFunc
	id      string
	body    string
	ctx     map[string]any
	store   func(s *store_mocks.MockStore)
	running []*model.Task
	want    int
	check   func(t *testing.T, s *store_mocks.MockStore, w *httptest.ResponseRecorder)
}

var errDB = errors.New("db down")

func runAgentCase(t *testing.T, tc agentCase) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	st := store_mocks.NewMockStore(t)
	if tc.store != nil {
		tc.store(st)
	}
	q := queue_mocks.NewMockQueue(t)
	q.On("Info", mock.Anything).Return(queue.InfoT{Running: tc.running}).Maybe()
	q.On("KickAgentWorkers", mock.Anything).Maybe()
	prev := server.Config.Services.Queue
	server.Config.Services.Queue = q
	t.Cleanup(func() { server.Config.Services.Queue = prev })

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("store", st)
	for k, v := range tc.ctx {
		c.Set(k, v)
	}
	c.Params = gin.Params{{Key: "agent_id", Value: tc.id}, {Key: "org_id", Value: tc.id}}
	c.Request, _ = http.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
	c.Request.Header.Set("Content-Type", "application/json")
	tc.handler(c)
	c.Writer.WriteHeaderNow()
	assert.Equal(t, tc.want, w.Code)
	if tc.check != nil {
		tc.check(t, st, w)
	}
}

func findReturns(a *model.Agent, err error) func(*store_mocks.MockStore) {
	return func(s *store_mocks.MockStore) { s.On("AgentFind", mock.Anything).Return(a, err) }
}

func TestAgentHandlers(t *testing.T) {
	org := &model.Org{ID: 7}
	user := &model.User{ID: 1}
	orgAgent := func() *model.Agent { return &model.Agent{ID: 5, Name: "org-agent", OrgID: 7} }
	cases := []agentCase{
		{name: "list error", handler: GetAgents, want: http.StatusInternalServerError,
			store: func(s *store_mocks.MockStore) { s.On("AgentList", mock.Anything).Return(nil, errDB) }},

		{name: "tasks bad id", handler: GetAgentTasks, id: "x", want: http.StatusBadRequest},
		{name: "tasks not found", handler: GetAgentTasks, id: "5", want: http.StatusNotFound,
			store: findReturns(nil, types.ErrRecordNotExist)},
		{name: "tasks lists only this agent's running tasks", handler: GetAgentTasks, id: "5", want: http.StatusOK,
			store: findReturns(&model.Agent{ID: 5}, nil), running: []*model.Task{{ID: "a", AgentID: 5}, {ID: "b", AgentID: 6}},
			check: func(t *testing.T, _ *store_mocks.MockStore, w *httptest.ResponseRecorder) {
				assert.Contains(t, w.Body.String(), `"id":"a"`)
				assert.NotContains(t, w.Body.String(), `"id":"b"`)
			}},

		{name: "patch bad body", handler: PatchAgent, id: "5", body: "{", want: http.StatusBadRequest},
		{name: "patch bad id", handler: PatchAgent, id: "x", body: "{}", want: http.StatusBadRequest},
		{name: "patch not found", handler: PatchAgent, id: "5", body: "{}", want: http.StatusNotFound,
			store: findReturns(nil, types.ErrRecordNotExist)},
		{name: "patch update conflict", handler: PatchAgent, id: "5", body: `{"name":"n"}`, want: http.StatusConflict,
			store: func(s *store_mocks.MockStore) {
				s.On("AgentFind", mock.Anything).Return(&model.Agent{ID: 5}, nil)
				s.On("AgentUpdate", mock.Anything).Return(errDB)
			}},

		{name: "post bad body", handler: PostAgent, body: "{", want: http.StatusBadRequest},
		{name: "post create error", handler: PostAgent, body: `{"name":"n"}`, want: http.StatusInternalServerError,
			ctx: map[string]any{"user": user}, store: func(s *store_mocks.MockStore) { s.On("AgentCreate", mock.Anything).Return(errDB) }},

		{name: "delete bad id", handler: DeleteAgent, id: "x", want: http.StatusBadRequest},
		{name: "delete not found", handler: DeleteAgent, id: "5", want: http.StatusNotFound,
			store: findReturns(nil, types.ErrRecordNotExist)},
		{name: "delete refused while running", handler: DeleteAgent, id: "5", want: http.StatusConflict,
			store: findReturns(&model.Agent{ID: 5}, nil), running: []*model.Task{{ID: "a", AgentID: 5}}},
		{name: "delete error", handler: DeleteAgent, id: "5", want: http.StatusInternalServerError,
			store: func(s *store_mocks.MockStore) {
				s.On("AgentFind", mock.Anything).Return(&model.Agent{ID: 5}, nil)
				s.On("AgentDelete", mock.Anything).Return(errDB)
			}},
		{name: "delete ok", handler: DeleteAgent, id: "5", want: http.StatusNoContent,
			store: func(s *store_mocks.MockStore) {
				s.On("AgentFind", mock.Anything).Return(&model.Agent{ID: 5}, nil)
				s.On("AgentDelete", mock.Anything).Return(nil)
			}},

		{name: "org post bad org id", handler: PostOrgAgent, id: "x", want: http.StatusBadRequest, ctx: map[string]any{"user": user}},
		{name: "org post bad body", handler: PostOrgAgent, id: "7", body: "{", want: http.StatusBadRequest, ctx: map[string]any{"user": user}},
		{name: "org post create error", handler: PostOrgAgent, id: "7", body: `{"name":"n"}`, want: http.StatusInternalServerError,
			ctx: map[string]any{"user": user}, store: func(s *store_mocks.MockStore) { s.On("AgentCreate", mock.Anything).Return(errDB) }},
		{name: "org post ok", handler: PostOrgAgent, id: "7", body: `{"name":"n"}`, want: http.StatusOK,
			ctx: map[string]any{"user": user}, store: func(s *store_mocks.MockStore) {
				s.On("AgentCreate", mock.MatchedBy(func(a *model.Agent) bool { return a.OrgID == 7 && a.Name == "n" })).Return(nil)
			}},

		{name: "org list error", handler: GetOrgAgents, want: http.StatusInternalServerError, ctx: map[string]any{"org": org},
			store: func(s *store_mocks.MockStore) { s.On("AgentListForOrg", int64(7), mock.Anything).Return(nil, errDB) }},
		{name: "org list ok", handler: GetOrgAgents, want: http.StatusOK, ctx: map[string]any{"org": org},
			store: func(s *store_mocks.MockStore) {
				s.On("AgentListForOrg", int64(7), mock.Anything).Return([]*model.Agent{orgAgent()}, nil)
			}},

		{name: "org patch bad id", handler: PatchOrgAgent, id: "x", want: http.StatusBadRequest, ctx: map[string]any{"org": org}},
		{name: "org patch not found", handler: PatchOrgAgent, id: "5", want: http.StatusNotFound, ctx: map[string]any{"org": org},
			store: findReturns(nil, types.ErrRecordNotExist)},
		{name: "org patch other org", handler: PatchOrgAgent, id: "5", want: http.StatusBadRequest, ctx: map[string]any{"org": org},
			store: findReturns(&model.Agent{ID: 5, OrgID: 8}, nil)},
		{name: "org patch bad body", handler: PatchOrgAgent, id: "5", body: "{", want: http.StatusBadRequest, ctx: map[string]any{"org": org},
			store: findReturns(orgAgent(), nil)},
		{name: "org patch update error", handler: PatchOrgAgent, id: "5", body: `{"name":"n"}`, want: http.StatusInternalServerError,
			ctx: map[string]any{"org": org}, store: func(s *store_mocks.MockStore) {
				s.On("AgentFind", mock.Anything).Return(orgAgent(), nil)
				s.On("AgentUpdate", mock.Anything).Return(errDB)
			}},
		// #440: the org variant had the same copy-both-fields bug.
		{name: "org cordon keeps the name", handler: PatchOrgAgent, id: "5", body: `{"no_schedule":true}`, want: http.StatusOK,
			ctx: map[string]any{"org": org}, store: func(s *store_mocks.MockStore) {
				s.On("AgentFind", mock.Anything).Return(orgAgent(), nil)
				s.On("AgentUpdate", mock.MatchedBy(func(a *model.Agent) bool { return a.Name == "org-agent" && a.NoSchedule })).Return(nil)
			}},

		{name: "org delete bad id", handler: DeleteOrgAgent, id: "x", want: http.StatusBadRequest, ctx: map[string]any{"org": org}},
		{name: "org delete not found", handler: DeleteOrgAgent, id: "5", want: http.StatusNotFound, ctx: map[string]any{"org": org},
			store: findReturns(nil, types.ErrRecordNotExist)},
		{name: "org delete other org", handler: DeleteOrgAgent, id: "5", want: http.StatusBadRequest, ctx: map[string]any{"org": org},
			store: findReturns(&model.Agent{ID: 5, OrgID: 8}, nil)},
		{name: "org delete refused while running", handler: DeleteOrgAgent, id: "5", want: http.StatusConflict, ctx: map[string]any{"org": org},
			store: findReturns(orgAgent(), nil), running: []*model.Task{{ID: "a", AgentID: 5}}},
		{name: "org delete error", handler: DeleteOrgAgent, id: "5", want: http.StatusInternalServerError, ctx: map[string]any{"org": org},
			store: func(s *store_mocks.MockStore) {
				s.On("AgentFind", mock.Anything).Return(orgAgent(), nil)
				s.On("AgentDelete", mock.Anything).Return(errDB)
			}},
		{name: "org delete ok", handler: DeleteOrgAgent, id: "5", want: http.StatusNoContent, ctx: map[string]any{"org": org},
			store: func(s *store_mocks.MockStore) {
				s.On("AgentFind", mock.Anything).Return(orgAgent(), nil)
				s.On("AgentDelete", mock.Anything).Return(nil)
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { runAgentCase(t, tc) })
	}
}
