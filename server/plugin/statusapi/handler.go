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
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// Handler implements plugin.StatusHook, providing REST endpoints for
// external agents to report workflow status back to the Woodpecker server.
// This replaces the gRPC Init/Update/Done calls for externally-dispatched work.
type Handler struct {
	token string
}

// New creates a Handler with the given bearer token for authentication.
func New(token string) *Handler {
	return &Handler{token: token}
}

func (h *Handler) Name() string { return "status-api" }

// RegisterRoutes adds init/update/done endpoints under the plugin's group.
// Routes: POST /workflows/:id/init, /workflows/:id/update, /workflows/:id/done
// Handlers use store.FromContext(c) to access the database.
func (h *Handler) RegisterRoutes(group *gin.RouterGroup) {
	group.Use(h.bearerAuth())

	wf := group.Group("/workflows/:id")
	{
		wf.POST("/init", handleInit)
		wf.POST("/update", handleUpdate)
		wf.POST("/done", handleDone)
	}
}

func (h *Handler) Close() error { return nil }

// bearerAuth validates the Authorization header against the configured token.
func (h *Handler) bearerAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		auth := c.GetHeader("Authorization")
		token := strings.TrimPrefix(auth, "Bearer ")
		if token == "" || token == auth || token != h.token {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		c.Next()
	}
}
