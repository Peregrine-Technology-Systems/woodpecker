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

func TestLevelFor(t *testing.T) {
	cases := []struct {
		name   string
		method string
		status int
		want   zerolog.Level
	}{
		// Any error status wins regardless of method — every failure stays
		// visible at the default INFO level (preserves #215).
		{"GET 500", http.MethodGet, http.StatusInternalServerError, zerolog.WarnLevel},
		{"GET 404", http.MethodGet, http.StatusNotFound, zerolog.WarnLevel},
		{"POST 400", http.MethodPost, http.StatusBadRequest, zerolog.WarnLevel},
		{"GET 400 (boundary)", http.MethodGet, http.StatusBadRequest, zerolog.WarnLevel},
		// Mutating methods at a success/redirect status → INFO (audit value;
		// absence must never be read as "filtered, not failed" — #215 webhooks).
		{"POST 204", http.MethodPost, http.StatusNoContent, zerolog.InfoLevel},
		{"PUT 200", http.MethodPut, http.StatusOK, zerolog.InfoLevel},
		{"PATCH 200", http.MethodPatch, http.StatusOK, zerolog.InfoLevel},
		{"DELETE 200", http.MethodDelete, http.StatusOK, zerolog.InfoLevel},
		{"POST 399 (boundary)", http.MethodPost, 399, zerolog.InfoLevel},
		// Successful reads → DEBUG (the ~95% poll flood gated off by default).
		{"GET 200", http.MethodGet, http.StatusOK, zerolog.DebugLevel},
		{"GET 304", http.MethodGet, http.StatusNotModified, zerolog.DebugLevel},
		{"HEAD 200", http.MethodHead, http.StatusOK, zerolog.DebugLevel},
		{"GET 399 (boundary)", http.MethodGet, 399, zerolog.DebugLevel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, levelFor(tc.method, tc.status))
		})
	}
}

func TestMiddleware_LogsRequestFields(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var buf bytes.Buffer
	orig := log.Logger
	// Debug-level logger so the successful GET line (now DEBUG, #276) is captured.
	log.Logger = zerolog.New(&buf).Level(zerolog.DebugLevel)
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
	assert.Equal(t, "debug", entry["level"], "successful GET must be DEBUG-gated (#276)")
	assert.Equal(t, "GET", entry["method"])
	assert.Equal(t, "/api/hook", entry["path"])
	assert.EqualValues(t, http.StatusOK, entry["status"])
	assert.Contains(t, entry, "latency_ms")
	assert.Contains(t, entry, "remote_ip")
	assert.Equal(t, "test-agent/1.0", entry["user_agent"])
}

func TestMiddleware_SuccessfulGetSuppressedAtInfoLevel(t *testing.T) {
	// The core #276 behavior: at the production default (INFO), the
	// high-frequency successful-GET poll flood produces NO log line.
	gin.SetMode(gin.TestMode)

	var buf bytes.Buffer
	orig := log.Logger
	log.Logger = zerolog.New(&buf).Level(zerolog.InfoLevel)
	t.Cleanup(func() { log.Logger = orig })

	r := gin.New()
	r.Use(Middleware())
	r.GET("/api/repos/13/pipelines", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/api/repos/13/pipelines", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Empty(t, buf.String(), "successful GET poll must be suppressed at INFO (#276)")
}

func TestMiddleware_ErrorStatusVisibleAtInfoLevel(t *testing.T) {
	// A failing GET must still surface at the default INFO level — error
	// visibility is the #215 value we must not lose to the #276 gate.
	gin.SetMode(gin.TestMode)

	var buf bytes.Buffer
	orig := log.Logger
	log.Logger = zerolog.New(&buf).Level(zerolog.InfoLevel)
	t.Cleanup(func() { log.Logger = orig })

	r := gin.New()
	r.Use(Middleware())
	r.GET("/api/repos/13/pipelines", func(c *gin.Context) { c.Status(http.StatusInternalServerError) })

	req := httptest.NewRequest(http.MethodGet, "/api/repos/13/pipelines", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var entry map[string]any
	require.NoError(t, json.Unmarshal([]byte(buf.String()), &entry))
	assert.Equal(t, "warn", entry["level"])
	assert.EqualValues(t, http.StatusInternalServerError, entry["status"])
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
	// INFO-level logger: a mutating POST must remain visible at the production
	// default even on a 2xx/3xx status (#215 webhook 204-vs-500 split, #276).
	log.Logger = zerolog.New(&buf).Level(zerolog.InfoLevel)
	t.Cleanup(func() { log.Logger = orig })

	r := gin.New()
	r.Use(Middleware())
	r.POST("/api/hook", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	req := httptest.NewRequest(http.MethodPost, "/api/hook", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var entry map[string]any
	require.NoError(t, json.Unmarshal([]byte(buf.String()), &entry))
	assert.Equal(t, "info", entry["level"])
	assert.EqualValues(t, http.StatusNoContent, entry["status"])
	assert.Equal(t, "POST", entry["method"])
}
