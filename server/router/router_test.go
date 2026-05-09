// Copyright 2026 Peregrine Technology Systems
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0

package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoad_DoesNotPanicOnRouteRegistration is the smoke test that would
// have caught the pl#367 deploy panic. Load() registers every route on
// the gin engine; gin's radix tree disallows catch-all wildcards (`*name`)
// as siblings of static or named routes. The pre-#189 implementation used
// `apiBase.Any("/*path", …)` for the #27 unknown-path 404 handler, which
// panicked at Load time as soon as `/api/queue/tasks/:task_id/priority`
// (#47) introduced a parameterized route under /api/.
//
// This test simply asserts Load returns without panicking. The previous
// state would have failed here with:
//
//	panic: catch-all wildcard '*path' in new path '/api/*path' conflicts
//	       with existing path segment 're' in existing prefix 'pi/re'
func TestLoad_DoesNotPanicOnRouteRegistration(t *testing.T) {
	// Reset the global config to the zero value so test order doesn't
	// matter. server.Config is a global; other tests may have left
	// state in it, but Load only reads Server.RootPath which defaults
	// to empty string (== root mount).
	gin.SetMode(gin.TestMode)
	noRoute := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("spa"))
	}

	require.NotPanics(t, func() {
		_ = Load(noRoute)
	}, "Load() must not panic — gin route conflicts surface as panic at register time")
}

// TestLoad_UnknownAPIPathReturns404JSON locks in the #27 contract using
// the engine-level NoRoute handler instead of the conflicting
// apiBase.Any("/*path") catch-all (#189).
func TestLoad_UnknownAPIPathReturns404JSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	spaCalled := false
	noRoute := func(w http.ResponseWriter, _ *http.Request) {
		spaCalled = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("spa"))
	}

	h := Load(noRoute)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/this-route-does-not-exist", nil)
	h.ServeHTTP(w, r)

	assert.Equal(t, http.StatusNotFound, w.Code, "unknown /api/* path should be 404")
	assert.Contains(t, w.Header().Get("Content-Type"), "application/json")
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "not found", body["error"])
	assert.Equal(t, "/api/this-route-does-not-exist", body["path"])
	assert.False(t, spaCalled, "SPA fallback must NOT fire for /api/* paths")
}

// TestLoad_UnknownNonAPIPathFallsThroughToSPA verifies non-/api unknown
// paths still hit the SPA handler.
func TestLoad_UnknownNonAPIPathFallsThroughToSPA(t *testing.T) {
	gin.SetMode(gin.TestMode)
	spaCalled := false
	noRoute := func(w http.ResponseWriter, _ *http.Request) {
		spaCalled = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("spa"))
	}

	h := Load(noRoute)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/some-spa-route", nil)
	h.ServeHTTP(w, r)

	assert.True(t, spaCalled, "non-/api unknown path should fall through to SPA")
	assert.Equal(t, http.StatusOK, w.Code)
}
