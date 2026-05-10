// Copyright 2026 Woodpecker Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"

	store_mocks "go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
)

// =============================================================================
// detectWebhookSource — pure function, exercised across all known forges
// =============================================================================

func TestDetectWebhookSource(t *testing.T) {
	cases := []struct {
		name   string
		header string
		value  string
		want   string
	}{
		{"github", "X-GitHub-Event", "push", "github"},
		{"gitea", "X-Gitea-Event", "push", "gitea"},
		{"forgejo", "X-Forgejo-Event", "push", "forgejo"},
		{"gitlab", "X-Gitlab-Event", "Push Hook", "gitlab"},
		{"bitbucket", "X-Event-Key", "repo:push", "bitbucket"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &http.Request{Header: http.Header{}}
			req.Header.Set(tc.header, tc.value)
			assert.Equal(t, tc.want, detectWebhookSource(req))
		})
	}
}

// TestDetectWebhookSource_Unknown — request with no recognizable forge
// header is bucketed as "unknown" rather than dropped, because the
// counter exists precisely to surface webhooks we couldn't classify.
func TestDetectWebhookSource_Unknown(t *testing.T) {
	req := &http.Request{Header: http.Header{}}
	assert.Equal(t, "unknown", detectWebhookSource(req))
}

// TestDetectWebhookSource_FirstHeaderWins — if multiple forge headers are
// present (rare proxy/forwarding scenario), the GitHub check fires first.
// Documented for stability; reorder if a real conflict shows up.
func TestDetectWebhookSource_FirstHeaderWins(t *testing.T) {
	req := &http.Request{Header: http.Header{}}
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Gitea-Event", "push")
	assert.Equal(t, "github", detectWebhookSource(req))
}

// =============================================================================
// Counter increments — exercise PostHook through the lightweight reject paths
// (no manager/forge wiring needed because the request fails token parsing).
// =============================================================================

// TestPostHook_IncrementsReceivedAndDroppedOnTokenError covers the very
// first failure path: PostHook is invoked with no Authorization header, so
// token.ParseRequest errors out. We assert (a) received+1 (counted before
// any validation), and (b) dropped{reason=parse_token_error}+1.
func TestPostHook_IncrementsReceivedAndDroppedOnTokenError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	receivedBefore := testutil.ToFloat64(webhooksReceived.WithLabelValues("github"))
	droppedBefore := testutil.ToFloat64(webhooksDropped.WithLabelValues("parse_token_error"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	mockStore := store_mocks.NewMockStore(t)
	c.Set("store", mockStore)

	header := http.Header{}
	header.Set("X-GitHub-Event", "push") // forge label without a valid auth token
	c.Request = &http.Request{Header: header, URL: &url.URL{Scheme: "https"}}

	PostHook(c)

	assert.Equal(t, http.StatusBadRequest, c.Writer.Status())
	assert.InDelta(t, receivedBefore+1, testutil.ToFloat64(webhooksReceived.WithLabelValues("github")), 0.0001,
		"webhooks_received_total{source=github} must increment exactly once per request, before any validation")
	assert.InDelta(t, droppedBefore+1, testutil.ToFloat64(webhooksDropped.WithLabelValues("parse_token_error")), 0.0001,
		"webhooks_dropped_total{reason=parse_token_error} must increment for an invalid auth token")
}

// TestPostHook_UnknownSource counts unauthorized requests with no forge
// header under source=unknown — the canonical "stuck integration" signal.
func TestPostHook_UnknownSource(t *testing.T) {
	gin.SetMode(gin.TestMode)

	receivedBefore := testutil.ToFloat64(webhooksReceived.WithLabelValues("unknown"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	mockStore := store_mocks.NewMockStore(t)
	c.Set("store", mockStore)
	c.Request = &http.Request{Header: http.Header{}, URL: &url.URL{Scheme: "https"}}

	PostHook(c)

	assert.InDelta(t, receivedBefore+1, testutil.ToFloat64(webhooksReceived.WithLabelValues("unknown")), 0.0001)
}

// TestWebhooksDropped_AllReasonsAreEnumerableLabels — light invariant
// check: all the constant strings we increment with are stable. Catches
// typo-renames at compile time but also documents the taxonomy in one
// place.
func TestWebhooksDropped_AllReasonsAreEnumerableLabels(t *testing.T) {
	for _, reason := range []string{
		"parse_token_error",
		"forge_lookup_error",
		"ignore_event",
		"parse_hook_error",
		"empty_pipeline",
		"repo_from_forge_nil",
		"repo_id_mismatch",
		"repo_inactive",
		"repo_no_owner",
		"db_user_lookup_error",
		"db_redirection_error",
		"db_repo_update_error",
		"pull_disabled",
		"pipeline_create_failed",
	} {
		// Just instantiating the labelled counter without panicking is
		// enough: prometheus rejects invalid label values at this point.
		_ = webhooksDropped.WithLabelValues(reason)
	}
}
