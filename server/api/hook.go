// Copyright 2022 Woodpecker Authors
// Copyright 2021 Informatyka Boguslawski sp. z o.o. sp.k., http://www.ib.pl/
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

package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog/log"

	"go.woodpecker-ci.org/woodpecker/v3/server"
	"go.woodpecker-ci.org/woodpecker/v3/server/forge"
	"go.woodpecker-ci.org/woodpecker/v3/server/forge/types"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/pipeline"
	"go.woodpecker-ci.org/woodpecker/v3/server/store"
	"go.woodpecker-ci.org/woodpecker/v3/shared/token"
)

// PostHook
//
//	@Summary	Incoming webhook from forge
//	@Router		/hook [post]
//	@Produce	plain
//	@Success	200
//	@Tags		System
//	@Param		hook	body	object	true	"the webhook payload; forge is automatically detected"
func PostHook(c *gin.Context) {
	// #191: count every webhook receipt before any validation, so
	// woodpecker_webhooks_received_total is comparable against the forge's
	// outbound delivery log (e.g. GitHub's webhook page). Source label is
	// derived from request headers each forge sends; "unknown" when no
	// match (also useful — those rows reveal stuck integrations).
	webhooksReceived.WithLabelValues(detectWebhookSource(c.Request)).Inc()

	_store := store.FromContext(c)

	//
	// 1. Check if the webhook is valid and authorized
	//

	var repo *model.Repo

	_, err := token.ParseRequest([]token.Type{token.HookToken}, c.Request, func(t *token.Token) (string, error) {
		var err error
		repo, err = getRepoFromToken(_store, t)
		if err != nil {
			return "", err
		}

		return repo.Hash, nil
	})
	if err != nil {
		webhooksDropped.WithLabelValues("parse_token_error").Inc()
		msg := "failure to parse token from hook"
		log.Error().Err(err).Msg(msg)
		c.String(http.StatusBadRequest, msg)
		return
	}

	// Note: a defensive `if repo == nil` check used to live here, but the
	// upstream callback at line 68 (`return repo.Hash, nil`) nil-panics
	// before this point if getRepoFromToken returns (nil, nil). The check
	// was unreachable in current code; if the callback is ever refactored
	// to be nil-safe, restore the guard with a webhooks_dropped reason.

	//
	// 1b. Short-circuit an exact duplicate delivery (#338)
	//
	// Read-only check, placed after token validation (so this only ever
	// short-circuits an authenticated request, never lets an unauthenticated
	// one skip auth) but before the expensive forge Hook() parsing +
	// pipeline.Create(), which is the actual work a redelivery would
	// otherwise duplicate. The ID is only ever marked seen on this
	// function's DEFINITE-success return (below) — never here, and never on
	// a transient-error path, so GitHub's own retry-on-5xx redelivery (#321)
	// keeps working. A missing delivery-ID header (non-GitHub forge, or
	// GitHub ever changes it) always falls through to normal processing —
	// this is best-effort defense-in-depth, not the only guard: cancel.go's
	// pipelineNeedsCancel also refuses to cancel a same-commit pipeline
	// regardless of whether this cache catches the duplicate.
	deliveryID := webhookDeliveryID(c.Request)
	if globalWebhookDedup.check(deliveryID, time.Now()) {
		webhooksDropped.WithLabelValues("duplicate_delivery").Inc()
		log.Info().Str("delivery_id", deliveryID).Msg("ignoring hook: duplicate delivery already processed")
		c.Status(http.StatusNoContent)
		return
	}

	_forge, err := server.Config.Services.Manager.ForgeFromRepo(repo)
	if err != nil {
		webhooksDropped.WithLabelValues("forge_lookup_error").Inc()
		log.Error().Err(err).Int64("repo-id", repo.ID).Msgf("Cannot get forge with id: %d", repo.ForgeID)
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	//
	// 2. Parse the webhook data
	//

	repoFromForge, pipelineFromForge, err := _forge.Hook(c, c.Request)
	if err != nil {
		if errors.Is(err, &types.ErrIgnoreEvent{}) {
			webhooksDropped.WithLabelValues("ignore_event").Inc()
			msg := fmt.Sprintf("forge driver: %s", err)
			log.Debug().Err(err).Msg(msg)
			c.String(http.StatusOK, msg)
			return
		}

		// A transient forge-API failure during hook parsing (GitHub 5xx /
		// rate-limit / timeout, surfaced as ErrTransientForge after the adapter's
		// bounded retries) is NOT a permanent bad request. Return 503 so the
		// forge's own webhook-retry redelivers, instead of a 400 that GitHub
		// treats as "never retry" and which permanently strands the CI trigger
		// (woodpecker#321).
		if errors.Is(err, &types.ErrTransientForge{}) {
			webhooksDropped.WithLabelValues("forge_transient_error").Inc()
			msg := "transient forge error while parsing hook, awaiting forge redelivery"
			log.Warn().Err(err).Msg(msg)
			c.String(http.StatusServiceUnavailable, msg)
			return
		}

		webhooksDropped.WithLabelValues("parse_hook_error").Inc()
		msg := "failure to parse hook"
		// A real forge delivery failed to parse (as opposed to the expected/ignored
		// event types above) — this drops a CI trigger, so it must be visible at
		// server default log level, not buried at Debug (#301).
		log.Error().Err(err).Msg(msg)
		c.String(http.StatusBadRequest, msg)
		return
	}

	if pipelineFromForge == nil {
		webhooksDropped.WithLabelValues("empty_pipeline").Inc()
		msg := "ignoring hook: hook parsing resulted in empty pipeline"
		log.Debug().Msg(msg)
		c.String(http.StatusOK, msg)
		return
	}
	if repoFromForge == nil {
		webhooksDropped.WithLabelValues("repo_from_forge_nil").Inc()
		msg := "failure to ascertain repo from hook"
		log.Debug().Msg(msg)
		c.String(http.StatusBadRequest, msg)
		return
	}

	//
	// 3. Check the repo from the token is matching the repo returned by the forge
	//

	if repo.ForgeRemoteID != repoFromForge.ForgeRemoteID {
		webhooksDropped.WithLabelValues("repo_id_mismatch").Inc()
		log.Warn().Msgf("ignoring hook: repo %s does not match the repo from the token", repo.FullName)
		c.String(http.StatusBadRequest, "failure to parse token from hook")
		return
	}

	//
	// 4. Check if the repo is active and has an owner
	//

	if !repo.IsActive {
		webhooksDropped.WithLabelValues("repo_inactive").Inc()
		log.Debug().Msgf("ignoring hook: repo %s is inactive", repoFromForge.FullName)
		c.Status(http.StatusNoContent)
		return
	}

	if repo.UserID == 0 {
		webhooksDropped.WithLabelValues("repo_no_owner").Inc()
		log.Warn().Msgf("ignoring hook. repo %s has no owner.", repo.FullName)
		c.Status(http.StatusNoContent)
		return
	}

	user, err := _store.GetUser(repo.UserID)
	if err != nil {
		webhooksDropped.WithLabelValues("db_user_lookup_error").Inc()
		handleDBError(c, err)
		return
	}
	forge.Refresh(c, _forge, _store, user)

	//
	// 4. Update the repo
	//

	if repo.FullName != repoFromForge.FullName {
		// create a redirection
		err = _store.CreateRedirection(&model.Redirection{RepoID: repo.ID, FullName: repo.FullName})
		if err != nil {
			webhooksDropped.WithLabelValues("db_redirection_error").Inc()
			_ = c.AbortWithError(http.StatusInternalServerError, err)
			return
		}
	}

	repo.Update(repoFromForge)
	err = _store.UpdateRepo(repo)
	if err != nil {
		webhooksDropped.WithLabelValues("db_repo_update_error").Inc()
		c.String(http.StatusInternalServerError, err.Error())
		return
	}

	//
	// 5. Check if pull requests are allowed for this repo
	//

	if pipelineFromForge.IsPullRequest() && !repo.AllowPull {
		webhooksDropped.WithLabelValues("pull_disabled").Inc()
		log.Debug().Str("repo", repo.FullName).Msg("ignoring hook: pull requests are disabled for this repo in woodpecker")
		c.Status(http.StatusNoContent)
		return
	}

	//
	// 6. Finally create a pipeline
	//

	pl, err := pipeline.Create(c, _store, repo, pipelineFromForge)
	if err != nil {
		if errors.Is(err, pipeline.ErrFiltered) {
			webhooksDropped.WithLabelValues("filtered").Inc()
		} else {
			webhooksDropped.WithLabelValues("pipeline_create_failed").Inc()
		}
		handlePipelineErr(c, err)
	} else {
		// #338: only mark the delivery seen once a pipeline was DEFINITELY
		// created — a failed attempt (including one that legitimately wants
		// a forge redelivery) must remain retryable.
		globalWebhookDedup.markSeen(deliveryID, time.Now())
		c.JSON(http.StatusOK, pl)
	}
}

func getRepoFromToken(store store.Store, t *token.Token) (*model.Repo, error) {
	if t.Get("repo-forge-remote-id") != "" {
		forgeID, err := strconv.ParseInt(t.Get("forge-id"), 10, 64)
		if err != nil {
			return nil, err
		}

		return store.GetRepoForgeID(forgeID, model.ForgeRemoteID(t.Get("repo-forge-remote-id")))
	}

	// get the repo by the repo-id
	// TODO: remove in next major
	repoID, err := strconv.ParseInt(t.Get("repo-id"), 10, 64)
	if err != nil {
		return nil, err
	}
	return store.GetRepo(repoID)
}
