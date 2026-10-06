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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"go.woodpecker-ci.org/woodpecker/v3/server"
	forge_mocks "go.woodpecker-ci.org/woodpecker/v3/server/forge/mocks"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/pubsub"
	manager_mocks "go.woodpecker-ci.org/woodpecker/v3/server/services/mocks"
	store_mocks "go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
)

// #388: every seat shares one forge login, so the stored cancel said "amalc" for
// the scaler, the healthcheck, a seat and a human alike. These drive the REAL
// handler and assert on what reaches the store — the one observation that says
// whether the attribution was recorded, rather than whether a helper behaves.

// cancelWith runs CancelPipeline against a running pipeline with the given raw
// query string and returns the status the handler set plus every pipeline row written.
func cancelWith(t *testing.T, rawQuery string) (int, []*model.Pipeline) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	running := &model.Pipeline{ID: 2, Number: 2, Status: model.StatusRunning}
	repo := &model.Repo{ID: 1}
	user := &model.User{Login: "testuser"}

	var written []*model.Pipeline
	mockForge := forge_mocks.NewMockForge(t)
	mockStore := store_mocks.NewMockStore(t)
	mockStore.On("GetPipelineNumber", repo, int64(2)).Return(running, nil).Maybe()
	mockStore.On("WorkflowGetTree", mock.Anything).Return([]*model.Workflow{}, nil).Maybe()
	mockStore.On("UpdatePipeline", mock.Anything).Run(func(args mock.Arguments) {
		written = append(written, args.Get(0).(*model.Pipeline))
	}).Return(nil).Maybe()

	mockManager := manager_mocks.NewMockManager(t)
	mockManager.On("ForgeFromRepo", repo).Return(mockForge, nil).Maybe()
	server.Config.Services.Manager = mockManager
	server.Config.Services.Pubsub = pubsub.New()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/cancel?"+rawQuery, nil)
	c.Set("store", mockStore)
	c.Set("repo", repo)
	c.Set("user", user)
	c.Params = gin.Params{{Key: "number", Value: "2"}}

	CancelPipeline(c)
	return c.Writer.Status(), written
}

func TestCancelPipelineRecordsActor(t *testing.T) {
	status, written := cancelWith(t, "source=seat.woodpecker")

	assert.Equal(t, http.StatusNoContent, status)
	if assert.Len(t, written, 1) && assert.NotNil(t, written[0].CancelInfo) {
		assert.Equal(t, "seat.woodpecker", written[0].CancelInfo.CanceledByActor)
		// the shared login is still recorded; the actor is ADDITIONAL, not a replacement
		assert.Equal(t, "testuser", written[0].CancelInfo.CanceledByUser)
	}
}

// Backwards compatible: callers that send nothing new behave exactly as before.
func TestCancelPipelineWithoutActorIsUnchanged(t *testing.T) {
	status, written := cancelWith(t, "")

	assert.Equal(t, http.StatusNoContent, status)
	if assert.Len(t, written, 1) && assert.NotNil(t, written[0].CancelInfo) {
		assert.Equal(t, "", written[0].CancelInfo.CanceledByActor)
		assert.Equal(t, "testuser", written[0].CancelInfo.CanceledByUser)
		assert.Equal(t, model.CancelTriggerUser, written[0].CancelInfo.Trigger)
	}
}

// The actor lives in cancel_info, NOT in kill_reason: kill_reason says which
// SERVER code path moved the pipeline, and a caller-declared value must not share
// that column. The invariant is that declaring an actor changes nothing about
// kill_reason or the derived trigger — compared against the same fixture without
// one, so it does not depend on which path the fixture happens to take.
func TestCancelPipelineActorDoesNotTouchKillReason(t *testing.T) {
	_, without := cancelWith(t, "")
	_, with := cancelWith(t, "source=healthcheck")

	if assert.Len(t, without, 1) && assert.Len(t, with, 1) {
		assert.Equal(t, without[0].KillReason, with[0].KillReason)
		assert.NotEqual(t, "healthcheck", with[0].KillReason)
		assert.Equal(t, without[0].CancelInfo.Trigger, with[0].CancelInfo.Trigger)
	}
}

// A malformed actor is refused loudly. Dropping it would make an attributed
// cancel look unattributed, and the cancel must NOT go ahead under a bad label.
func TestCancelPipelineRejectsMalformedActor(t *testing.T) {
	for _, q := range []string{"source=Bad+Actor", "source=seat%0AINFO+forged", "source=a"} {
		t.Run(q, func(t *testing.T) {
			status, written := cancelWith(t, q)
			assert.Equal(t, http.StatusBadRequest, status)
			assert.Empty(t, written, "nothing may be cancelled under a rejected label")
		})
	}
}
