// Copyright 2026 Woodpecker Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0

package accesslog

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMiddleware_LogsRequestFields(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var buf bytes.Buffer
	orig := log.Logger
	log.Logger = zerolog.New(&buf)
	t.Cleanup(func() { log.Logger = orig })

	r := gin.New()
	r.Use(Middleware())
	r.GET("/api/hook", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/api/hook", nil)
	req.Header.Set("User-Agent", "test-agent/1.0")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.NotEmpty(t, buf.String(), "expected at least one log line")
	var entry map[string]any
	require.NoError(t, json.Unmarshal([]byte(buf.String()), &entry))

	assert.Equal(t, "http", entry["message"])
	assert.Equal(t, "GET", entry["method"])
	assert.Equal(t, "/api/hook", entry["path"])
	assert.EqualValues(t, http.StatusOK, entry["status"])
	assert.Contains(t, entry, "latency_ms")
	assert.Contains(t, entry, "remote_ip")
	assert.Equal(t, "test-agent/1.0", entry["user_agent"])
}

func TestMiddleware_SkipsHealthz(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var buf bytes.Buffer
	orig := log.Logger
	log.Logger = zerolog.New(&buf)
	t.Cleanup(func() { log.Logger = orig })

	r := gin.New()
	r.Use(Middleware())
	r.GET("/healthz", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Empty(t, buf.String(), "/healthz must not produce an access log line")
}

func TestMiddleware_SkipsMetrics(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var buf bytes.Buffer
	orig := log.Logger
	log.Logger = zerolog.New(&buf)
	t.Cleanup(func() { log.Logger = orig })

	r := gin.New()
	r.Use(Middleware())
	r.GET("/metrics", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Empty(t, buf.String(), "/metrics must not produce an access log line")
}

func TestMiddleware_LogsNonOKStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var buf bytes.Buffer
	orig := log.Logger
	log.Logger = zerolog.New(&buf)
	t.Cleanup(func() { log.Logger = orig })

	r := gin.New()
	r.Use(Middleware())
	r.POST("/api/hook", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	req := httptest.NewRequest(http.MethodPost, "/api/hook", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var entry map[string]any
	require.NoError(t, json.Unmarshal([]byte(buf.String()), &entry))
	assert.EqualValues(t, http.StatusNoContent, entry["status"])
	assert.Equal(t, "POST", entry["method"])
}
