// Copyright 2018 Drone.IO Inc.
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

package router

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog/log"
	openapi_files "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"

	"go.woodpecker-ci.org/woodpecker/v3/cmd/server/openapi"
	"go.woodpecker-ci.org/woodpecker/v3/server"
	"go.woodpecker-ci.org/woodpecker/v3/server/api"
	"go.woodpecker-ci.org/woodpecker/v3/server/api/metrics"
	"go.woodpecker-ci.org/woodpecker/v3/server/router/middleware/header"
	"go.woodpecker-ci.org/woodpecker/v3/server/router/middleware/session"
	"go.woodpecker-ci.org/woodpecker/v3/server/router/middleware/token"
	"go.woodpecker-ci.org/woodpecker/v3/server/web"
)

// Load loads the router.
func Load(noRouteHandler http.HandlerFunc, middleware ...gin.HandlerFunc) http.Handler {
	e := gin.New()
	e.UseRawPath = true
	e.Use(gin.Recovery())

	e.Use(func(c *gin.Context) {
		log.Trace().Msgf("[%s] %s", c.Request.Method, c.Request.URL.String())
		c.Next()
	})

	e.Use(header.NoCache)
	e.Use(header.Options)
	e.Use(header.Secure)
	e.Use(middleware...)
	e.Use(session.SetUser())
	e.Use(token.Refresh)

	// Unknown /api/* paths return 404 JSON; everything else falls through
	// to the SPA handler (web/dist index.html). This replaces the previous
	// `apiBase.Any("/*path", …)` catch-all (#27), which conflicted with
	// gin's tree when /api/ had any parameterized routes — gin disallows
	// catch-all wildcards as siblings of named/static routes at the same
	// node, so adding e.g. `PATCH /api/queue/tasks/:task_id/priority`
	// caused the server to panic at startup with
	//   "catch-all wildcard '*path' in new path '/api/*path' conflicts
	//    with existing path segment 're' in existing prefix 'pi/re'"
	// Reference: pl#367 deploy panic 2026-05-09 20:32:51 UTC. (#27 / #189)
	e.NoRoute(func(c *gin.Context) {
		if strings.HasPrefix(c.Request.URL.Path, "/api/") {
			c.JSON(http.StatusNotFound, gin.H{
				"error": "not found",
				"path":  c.Request.URL.Path,
			})
			return
		}
		noRouteHandler(c.Writer, c.Request)
	})

	base := e.Group(server.Config.Server.RootPath)
	{
		base.GET("/web-config.js", web.Config)

		base.GET("/logout", api.GetLogout)
		auth := base.Group("/authorize")
		{
			auth.GET("", api.HandleAuth)
			auth.POST("", api.HandleAuth)
		}

		base.GET("/metrics", metrics.PromHandler())
		base.GET("/version", api.Version)
		base.GET("/healthz", api.Health)
		base.GET("/ws/agent", api.WSAgent) // Full WebSocket agent transport (#474)
	}

	apiRoutes(base)
	if server.Config.WebUI.EnableSwagger {
		setupSwaggerConfigAndRoutes(e)
	}

	return e
}

func setupSwaggerConfigAndRoutes(e *gin.Engine) {
	openapi.SwaggerInfo.Host = getHost(server.Config.Server.Host)
	openapi.SwaggerInfo.BasePath = server.Config.Server.RootPath + "/api"
	e.GET(server.Config.Server.RootPath+"/swagger/*any", ginSwagger.WrapHandler(openapi_files.Handler))
}

func getHost(s string) string {
	parse, _ := url.Parse(s)
	return parse.Host
}
