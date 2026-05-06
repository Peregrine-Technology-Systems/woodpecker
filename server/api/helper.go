// Copyright 2022 Woodpecker Authors
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
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog/log"

	"go.woodpecker-ci.org/woodpecker/v3/server"
	"go.woodpecker-ci.org/woodpecker/v3/server/forge"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/pipeline"
	"go.woodpecker-ci.org/woodpecker/v3/server/store"
	"go.woodpecker-ci.org/woodpecker/v3/server/store/datastore"
	"go.woodpecker-ci.org/woodpecker/v3/server/store/types"
)

// retryAfterSeconds is the Retry-After value returned with 503 when the write
// queue is full. GitHub webhook delivery retries on 503 and respects this header.
const retryAfterSeconds = "5"

// abort503IfOverloaded checks whether err is ErrWriteQueueFull and, if so,
// responds with HTTP 503 + Retry-After and returns true. Callers should return
// immediately when this returns true. Otherwise returns false (caller handles).
func abort503IfOverloaded(c *gin.Context, err error) bool {
	if !errors.Is(err, datastore.ErrWriteQueueFull) {
		return false
	}
	c.Header("Retry-After", retryAfterSeconds)
	c.String(http.StatusServiceUnavailable, fmt.Sprintf("server temporarily overloaded — retry in %s seconds", retryAfterSeconds))
	return true
}

func handlePipelineErr(c *gin.Context, err error) {
	switch {
	case abort503IfOverloaded(c, err):
		// 503 already sent
	case errors.Is(err, &pipeline.ErrNotFound{}):
		c.String(http.StatusNotFound, "%s", err)
	case errors.Is(err, &pipeline.ErrBadRequest{}):
		c.String(http.StatusBadRequest, "%s", err)
	case errors.Is(err, pipeline.ErrFiltered):
		// for debugging purpose we add a header
		c.Writer.Header().Add("Pipeline-Filtered", "true")
		c.Status(http.StatusNoContent)
	default:
		_ = c.AbortWithError(http.StatusInternalServerError, err)
	}
}

func handleDBError(c *gin.Context, err error) {
	if abort503IfOverloaded(c, err) {
		return
	}
	if errors.Is(err, types.ErrRecordNotExist) {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	_ = c.AbortWithError(http.StatusInternalServerError, err)
}

// If the forge has a refresh token, the current access token may be stale.
// Therefore, we should refresh prior to dispatching the job.
func refreshUserToken(c *gin.Context, user *model.User) {
	_store := store.FromContext(c)
	_forge, err := server.Config.Services.Manager.ForgeFromUser(user)
	if err != nil {
		log.Error().Err(err).Msg("Cannot get forge from user")
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	forge.Refresh(c, _forge, _store, user)
}

// pipelineDeleteAllowed checks if the given pipeline can be deleted based on its status.
// It returns a bool indicating if delete is allowed, and the pipeline's status.
func pipelineDeleteAllowed(pl *model.Pipeline) bool {
	switch pl.Status {
	case model.StatusRunning, model.StatusPending, model.StatusBlocked:
		return false
	}

	return true
}
