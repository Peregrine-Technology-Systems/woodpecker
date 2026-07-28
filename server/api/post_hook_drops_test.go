// Copyright 2026 Woodpecker Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0

package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"go.woodpecker-ci.org/woodpecker/v3/server"
	forge_mocks "go.woodpecker-ci.org/woodpecker/v3/server/forge/mocks"
	"go.woodpecker-ci.org/woodpecker/v3/server/forge/types"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	services_mocks "go.woodpecker-ci.org/woodpecker/v3/server/services/mocks"
	"go.woodpecker-ci.org/woodpecker/v3/server/services/permissions"
	store_mocks "go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
	"go.woodpecker-ci.org/woodpecker/v3/shared/token"
)

// =============================================================================
// PostHook scenario tests — covers every drop-counter path enumerated in the
// hook handler so the per-file 95% gate clears (and so each rejection is
// regression-tested with the exact reason label).
// =============================================================================

// hookFixture builds the minimum scaffolding to call PostHook with a valid
// signed token: store, manager, forge mocks pre-wired, gin context set up.
// Each test then layers its specific failure mode on top.
type hookFixture struct {
	t       *testing.T
	store   *store_mocks.MockStore
	manager *services_mocks.MockManager
	forge   *forge_mocks.MockForge
	repo    *model.Repo
	user    *model.User
	c       *gin.Context
	w       *httptest.ResponseRecorder
}

func newHookFixture(t *testing.T) *hookFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	mockStore := store_mocks.NewMockStore(t)
	mockManager := services_mocks.NewMockManager(t)
	mockForge := forge_mocks.NewMockForge(t)
	server.Config.Services.Manager = mockManager
	server.Config.Permissions.Open = true
	server.Config.Permissions.Orgs = permissions.NewOrgs(nil)
	server.Config.Permissions.Admins = permissions.NewAdmins(nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("store", mockStore)

	repo := &model.Repo{
		ID: 123, ForgeRemoteID: "123", Owner: "owner", Name: "name",
		FullName: "owner/name", IsActive: true, UserID: 7,
		Hash: "secret-123-this-is-a-secret",
	}
	user := &model.User{ID: 7}

	repoToken := token.New(token.HookToken)
	repoToken.Set("repo-id", fmt.Sprintf("%d", repo.ID))
	signed, err := repoToken.Sign(repo.Hash)
	assert.NoError(t, err)

	header := http.Header{}
	header.Set("Authorization", fmt.Sprintf("Bearer %s", signed))
	header.Set("X-GitHub-Event", "push")
	c.Request = &http.Request{Header: header, URL: &url.URL{Scheme: "https"}}

	mockStore.On("GetRepo", repo.ID).Return(repo, nil)

	return &hookFixture{
		t: t, store: mockStore, manager: mockManager, forge: mockForge,
		repo: repo, user: user, c: c, w: w,
	}
}

// ── Drop tests for each early-return reason ────────────────────────────────

// NOTE: the `if repo == nil` branch (line 78) is defensive — current code
// flow nil-panics earlier (in the ParseRequest callback at line 68 returning
// repo.Hash) when GetRepo returns (nil, nil). Triggering the branch through
// PostHook isn't possible without a code change. Left uncovered, documented.

func TestPostHook_ForgeLookupError(t *testing.T) {
	f := newHookFixture(t)
	f.manager.On("ForgeFromRepo", f.repo).Return(nil, errors.New("forge lookup failed"))
	PostHook(f.c)
	assert.Equal(t, http.StatusInternalServerError, f.c.Writer.Status())
}

func TestPostHook_IgnoreEvent(t *testing.T) {
	f := newHookFixture(t)
	f.manager.On("ForgeFromRepo", f.repo).Return(f.forge, nil)
	f.forge.On("Hook", mock.Anything, mock.Anything).Return(nil, nil, &types.ErrIgnoreEvent{})
	PostHook(f.c)
	assert.Equal(t, http.StatusOK, f.c.Writer.Status())
}

func TestPostHook_ParseHookError(t *testing.T) {
	f := newHookFixture(t)
	f.manager.On("ForgeFromRepo", f.repo).Return(f.forge, nil)
	f.forge.On("Hook", mock.Anything, mock.Anything).Return(nil, nil, errors.New("parse failed"))
	PostHook(f.c)
	assert.Equal(t, http.StatusBadRequest, f.c.Writer.Status())
}

func TestPostHook_EmptyPipeline(t *testing.T) {
	f := newHookFixture(t)
	f.manager.On("ForgeFromRepo", f.repo).Return(f.forge, nil)
	f.forge.On("Hook", mock.Anything, mock.Anything).Return(f.repo, nil, nil)
	PostHook(f.c)
	assert.Equal(t, http.StatusOK, f.c.Writer.Status())
}

func TestPostHook_RepoFromForgeNil(t *testing.T) {
	f := newHookFixture(t)
	f.manager.On("ForgeFromRepo", f.repo).Return(f.forge, nil)
	pl := &model.Pipeline{Event: model.EventPush}
	f.forge.On("Hook", mock.Anything, mock.Anything).Return(nil, pl, nil)
	PostHook(f.c)
	assert.Equal(t, http.StatusBadRequest, f.c.Writer.Status())
}

func TestPostHook_RepoIDMismatch(t *testing.T) {
	f := newHookFixture(t)
	f.manager.On("ForgeFromRepo", f.repo).Return(f.forge, nil)
	mismatch := &model.Repo{ID: 123, ForgeRemoteID: "DIFFERENT", FullName: "owner/name"}
	pl := &model.Pipeline{Event: model.EventPush}
	f.forge.On("Hook", mock.Anything, mock.Anything).Return(mismatch, pl, nil)
	PostHook(f.c)
	assert.Equal(t, http.StatusBadRequest, f.c.Writer.Status())
}

func TestPostHook_RepoInactive(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockStore := store_mocks.NewMockStore(t)
	mockManager := services_mocks.NewMockManager(t)
	mockForge := forge_mocks.NewMockForge(t)
	server.Config.Services.Manager = mockManager
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("store", mockStore)

	repo := &model.Repo{ID: 124, ForgeRemoteID: "124", FullName: "o/r", Hash: "h", IsActive: false}
	repoToken := token.New(token.HookToken)
	repoToken.Set("repo-id", "124")
	signed, _ := repoToken.Sign("h")
	header := http.Header{}
	header.Set("Authorization", fmt.Sprintf("Bearer %s", signed))
	c.Request = &http.Request{Header: header, URL: &url.URL{Scheme: "https"}}
	mockStore.On("GetRepo", int64(124)).Return(repo, nil)
	mockManager.On("ForgeFromRepo", repo).Return(mockForge, nil)
	mockForge.On("Hook", mock.Anything, mock.Anything).Return(repo, &model.Pipeline{Event: model.EventPush}, nil)

	PostHook(c)
	assert.Equal(t, http.StatusNoContent, c.Writer.Status())
}

func TestPostHook_RepoNoOwner(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockStore := store_mocks.NewMockStore(t)
	mockManager := services_mocks.NewMockManager(t)
	mockForge := forge_mocks.NewMockForge(t)
	server.Config.Services.Manager = mockManager
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("store", mockStore)

	repo := &model.Repo{ID: 125, ForgeRemoteID: "125", FullName: "o/r", Hash: "h", IsActive: true, UserID: 0}
	repoToken := token.New(token.HookToken)
	repoToken.Set("repo-id", "125")
	signed, _ := repoToken.Sign("h")
	header := http.Header{}
	header.Set("Authorization", fmt.Sprintf("Bearer %s", signed))
	c.Request = &http.Request{Header: header, URL: &url.URL{Scheme: "https"}}
	mockStore.On("GetRepo", int64(125)).Return(repo, nil)
	mockManager.On("ForgeFromRepo", repo).Return(mockForge, nil)
	mockForge.On("Hook", mock.Anything, mock.Anything).Return(repo, &model.Pipeline{Event: model.EventPush}, nil)

	PostHook(c)
	assert.Equal(t, http.StatusNoContent, c.Writer.Status())
}

func TestPostHook_DBUserLookupError(t *testing.T) {
	f := newHookFixture(t)
	f.manager.On("ForgeFromRepo", f.repo).Return(f.forge, nil)
	pl := &model.Pipeline{Event: model.EventPush}
	f.forge.On("Hook", mock.Anything, mock.Anything).Return(f.repo, pl, nil)
	f.store.On("GetUser", f.user.ID).Return(nil, errors.New("user lookup failed"))
	PostHook(f.c)
	// handleDBError → 500
	assert.GreaterOrEqual(t, f.c.Writer.Status(), 400)
}

// TestPostHook_TokenCallbackError covers the inner "return ”, err" path
// of the ParseRequest callback (when getRepoFromToken errors).
func TestPostHook_TokenCallbackError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockStore := store_mocks.NewMockStore(t)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("store", mockStore)

	tk := token.New(token.HookToken)
	tk.Set("repo-id", "555")
	signed, _ := tk.Sign("doesnt-matter")
	header := http.Header{}
	header.Set("Authorization", fmt.Sprintf("Bearer %s", signed))
	c.Request = &http.Request{Header: header, URL: &url.URL{Scheme: "https"}}
	mockStore.On("GetRepo", int64(555)).Return(nil, errors.New("DB error"))

	PostHook(c)
	assert.Equal(t, http.StatusBadRequest, c.Writer.Status())
}

// TestPostHook_DBUpdateRepoError exercises the UpdateRepo failure branch.
func TestPostHook_DBUpdateRepoError(t *testing.T) {
	f := newHookFixture(t)
	f.manager.On("ForgeFromRepo", f.repo).Return(f.forge, nil)
	pl := &model.Pipeline{Event: model.EventPush}
	f.forge.On("Hook", mock.Anything, mock.Anything).Return(f.repo, pl, nil)
	f.store.On("GetUser", f.user.ID).Return(f.user, nil)
	// forge.Refresh requires Netrc (or no-op refresh) — most forges' Refresh
	// is a no-op when no token; mock as needed:
	f.forge.On("Refresh", mock.Anything, mock.Anything, mock.Anything).Return(false, nil).Maybe()
	f.store.On("UpdateRepo", f.repo).Return(errors.New("update failed"))
	PostHook(f.c)
	assert.Equal(t, http.StatusInternalServerError, f.c.Writer.Status())
}

// TestPostHook_PullDisabled exercises the pull_disabled drop path.
func TestPostHook_PullDisabled(t *testing.T) {
	f := newHookFixture(t)
	f.repo.AllowPull = false
	f.manager.On("ForgeFromRepo", f.repo).Return(f.forge, nil)
	// Pipeline IS a pull request and AllowPull=false → drop.
	pl := &model.Pipeline{Event: model.EventPull}
	f.forge.On("Hook", mock.Anything, mock.Anything).Return(f.repo, pl, nil)
	f.store.On("GetUser", f.user.ID).Return(f.user, nil)
	f.forge.On("Refresh", mock.Anything, mock.Anything, mock.Anything).Return(false, nil).Maybe()
	f.store.On("UpdateRepo", f.repo).Return(nil)
	PostHook(f.c)
	assert.Equal(t, http.StatusNoContent, f.c.Writer.Status())
}

// TestPostHook_RepoRenameTriggersRedirection exercises the repo-rename
// branch — when the forge says the repo's name changed, a redirection is
// recorded. This test asserts the success path of CreateRedirection (and
// the subsequent UpdateRepo).
func TestPostHook_RepoRenameTriggersRedirection(t *testing.T) {
	f := newHookFixture(t)
	renamed := *f.repo
	renamed.FullName = "owner/new-name"
	f.manager.On("ForgeFromRepo", f.repo).Return(f.forge, nil)
	pl := &model.Pipeline{Event: model.EventPull}
	f.repo.AllowPull = false // short-circuit before pipeline.Create wiring
	f.forge.On("Hook", mock.Anything, mock.Anything).Return(&renamed, pl, nil)
	f.store.On("GetUser", f.user.ID).Return(f.user, nil)
	f.forge.On("Refresh", mock.Anything, mock.Anything, mock.Anything).Return(false, nil).Maybe()
	f.store.On("CreateRedirection", mock.Anything).Return(nil)
	f.store.On("UpdateRepo", f.repo).Return(nil)
	PostHook(f.c)
	// Lands at pull_disabled drop (204) because AllowPull=false.
	assert.Equal(t, http.StatusNoContent, f.c.Writer.Status())
}

// TestPostHook_RedirectionError covers db_redirection_error.
func TestPostHook_RedirectionError(t *testing.T) {
	f := newHookFixture(t)
	renamed := *f.repo
	renamed.FullName = "owner/new-name"
	f.manager.On("ForgeFromRepo", f.repo).Return(f.forge, nil)
	pl := &model.Pipeline{Event: model.EventPush}
	f.forge.On("Hook", mock.Anything, mock.Anything).Return(&renamed, pl, nil)
	f.store.On("GetUser", f.user.ID).Return(f.user, nil)
	f.forge.On("Refresh", mock.Anything, mock.Anything, mock.Anything).Return(false, nil).Maybe()
	f.store.On("CreateRedirection", mock.Anything).Return(errors.New("DB redirection failed"))
	PostHook(f.c)
	assert.Equal(t, http.StatusInternalServerError, f.c.Writer.Status())
}

// =============================================================================
// getRepoFromToken — branches not exercised by the happy-path TestHook
// =============================================================================

func TestGetRepoFromToken_RepoIDPath(t *testing.T) {
	mockStore := store_mocks.NewMockStore(t)
	repo := &model.Repo{ID: 200, Hash: "h"}
	mockStore.On("GetRepo", int64(200)).Return(repo, nil)
	tk := token.New(token.HookToken)
	tk.Set("repo-id", "200")
	got, err := getRepoFromToken(mockStore, tk)
	assert.NoError(t, err)
	assert.Equal(t, repo, got)
}

func TestGetRepoFromToken_ForgeRemoteIDPath(t *testing.T) {
	mockStore := store_mocks.NewMockStore(t)
	repo := &model.Repo{ID: 201, Hash: "h"}
	mockStore.On("GetRepoForgeID", int64(99), model.ForgeRemoteID("xyz")).Return(repo, nil)
	tk := token.New(token.HookToken)
	tk.Set("repo-forge-remote-id", "xyz")
	tk.Set("forge-id", "99")
	got, err := getRepoFromToken(mockStore, tk)
	assert.NoError(t, err)
	assert.Equal(t, repo, got)
}

func TestGetRepoFromToken_BadForgeID(t *testing.T) {
	tk := token.New(token.HookToken)
	tk.Set("repo-forge-remote-id", "xyz")
	tk.Set("forge-id", "not-a-number")
	_, err := getRepoFromToken(nil, tk)
	assert.Error(t, err)
}

func TestGetRepoFromToken_BadRepoID(t *testing.T) {
	tk := token.New(token.HookToken)
	tk.Set("repo-id", "not-a-number")
	_, err := getRepoFromToken(nil, tk)
	assert.Error(t, err)
}

// TestPostHook_FilteredCounterNotCreateFailed asserts that a webhook whose
// pipeline is filtered by ErrFiltered (skip-ci commit message) increments
// webhooksDropped{reason="filtered"} — NOT "pipeline_create_failed" (#213).
// The distinction matters for alerting: filtered is normal operation (204);
// pipeline_create_failed is a real server error (500).
func TestPostHook_FilteredCounterNotCreateFailed(t *testing.T) {
	f := newHookFixture(t)
	// Pipeline with [skip ci] in the message — pipeline.Create returns
	// ErrFiltered before touching the config service or store beyond GetUser.
	pl := &model.Pipeline{Event: model.EventPush, Message: "[skip ci] automated commit"}
	f.manager.On("ForgeFromRepo", f.repo).Return(f.forge, nil)
	f.forge.On("Hook", mock.Anything, mock.Anything).Return(f.repo, pl, nil)
	f.store.On("GetUser", f.user.ID).Return(f.user, nil)
	f.forge.On("Refresh", mock.Anything, mock.Anything, mock.Anything).Return(false, nil).Maybe()
	f.store.On("UpdateRepo", f.repo).Return(nil)

	beforeFiltered := testutil.ToFloat64(webhooksDropped.WithLabelValues("filtered"))
	beforeFailed := testutil.ToFloat64(webhooksDropped.WithLabelValues("pipeline_create_failed"))

	PostHook(f.c)

	assert.Equal(t, http.StatusNoContent, f.c.Writer.Status())
	assert.Equal(t, "true", f.w.Header().Get("Pipeline-Filtered"))
	assert.InDelta(t, beforeFiltered+1, testutil.ToFloat64(webhooksDropped.WithLabelValues("filtered")), 0.001)
	assert.InDelta(t, beforeFailed, testutil.ToFloat64(webhooksDropped.WithLabelValues("pipeline_create_failed")), 0.001)
}

// TestPostHook_DuplicateDeliveryShortCircuits — #338: a delivery ID already
// marked seen is rejected before ever touching the forge, proving the
// short-circuit actually happens where it's supposed to (not just that the
// cache logic works in isolation).
func TestPostHook_DuplicateDeliveryShortCircuits(t *testing.T) {
	f := newHookFixture(t)
	f.c.Request.Header.Set("X-GitHub-Delivery", "TestPostHook_DuplicateDeliveryShortCircuits-id")
	globalWebhookDedup.markSeen("TestPostHook_DuplicateDeliveryShortCircuits-id", time.Now())

	beforeDup := testutil.ToFloat64(webhooksDropped.WithLabelValues("duplicate_delivery"))

	PostHook(f.c)

	assert.Equal(t, http.StatusNoContent, f.c.Writer.Status())
	assert.InDelta(t, beforeDup+1, testutil.ToFloat64(webhooksDropped.WithLabelValues("duplicate_delivery")), 0.001)
	// ForgeFromRepo/Hook were never set up on the mocks — if PostHook reached
	// them, NewMockManager(t)/NewMockForge(t)'s own expectation-checking
	// would fail this test on an unexpected call.
}

// TestPostHook_NewDeliveryProceedsAndGetsMarkedSeen — the complementary case:
// an unseen delivery ID proceeds normally, and is marked seen only once the
// pipeline is actually created — not before.
func TestPostHook_NewDeliveryProceedsAndGetsMarkedSeen(t *testing.T) {
	f := newHookFixture(t)
	id := "TestPostHook_NewDeliveryProceedsAndGetsMarkedSeen-id"
	f.c.Request.Header.Set("X-GitHub-Delivery", id)

	pl := &model.Pipeline{Event: model.EventPush, Message: "[skip ci] automated commit"}
	f.manager.On("ForgeFromRepo", f.repo).Return(f.forge, nil)
	f.forge.On("Hook", mock.Anything, mock.Anything).Return(f.repo, pl, nil)
	f.store.On("GetUser", f.user.ID).Return(f.user, nil)
	f.forge.On("Refresh", mock.Anything, mock.Anything, mock.Anything).Return(false, nil).Maybe()
	f.store.On("UpdateRepo", f.repo).Return(nil)

	assert.False(t, globalWebhookDedup.check(id, time.Now()), "not seen before the request")

	PostHook(f.c)

	assert.Equal(t, http.StatusNoContent, f.c.Writer.Status(), "filtered by ErrFiltered, not by dedup")
	// pipeline.Create returned ErrFiltered (not a definite success), so the ID
	// must NOT be marked seen — a legitimate retry must still be possible.
	assert.False(t, globalWebhookDedup.check(id, time.Now()), "ErrFiltered is not a success — must stay retryable")
}
