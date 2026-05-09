// Copyright 2026 Peregrine Technology Systems
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0

package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog/log"

	"go.woodpecker-ci.org/woodpecker/v3/server"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/pubsub"
	"go.woodpecker-ci.org/woodpecker/v3/server/queue"
	"go.woodpecker-ci.org/woodpecker/v3/server/router/middleware/session"
	"go.woodpecker-ci.org/woodpecker/v3/server/store"
)

// taskPriorityChangedEventType is the value of the `type` field on the SSE
// payload — consumers (peregrine-queue-ui#4) discriminate priority-change
// events from existing pipeline events by this discriminator. (#47)
const taskPriorityChangedEventType = "task_priority_changed"

// authRequestEmailHeader is the header name oauth2-proxy sets per its
// canonical configuration. Used so the queue-ui proxy can attribute audit
// rows to the human SSO user even though the upstream Woodpecker request
// is authenticated with a single shared admin token.
const authRequestEmailHeader = "X-Auth-Request-Email"

// taskPriorityChangedPayload is the SSE event body. JSON-serialised onto
// the existing /api/stream/events channel; the event is tagged
// `private: "false"` so all connected SSE clients see it regardless of
// their per-repo permissions — the audit trail is the design's only
// authorisation check, so visibility is intentionally global.
type taskPriorityChangedPayload struct {
	Type        string `json:"type"`
	TaskID      string `json:"task_id"`
	OldPriority int64  `json:"old_priority"`
	NewPriority int64  `json:"new_priority"`
	ActorEmail  string `json:"actor_email"`
}

type patchPriorityRequest struct {
	Priority int64 `json:"priority"`
}

// PatchTaskPriority
//
//	@Summary		Update a queue task's priority
//	@Description	Reorders a pending task by mutating its priority. Higher values dispatch first; FIFO within a priority bucket. Writes a priority_audit row and emits a task_priority_changed SSE event.
//	@Router			/queue/tasks/{task_id}/priority [patch]
//	@Produce		json
//	@Param			task_id			path	string					true	"the task ID"
//	@Param			body			body	patchPriorityRequest	true	"new priority"
//	@Success		200	{object}	taskPriorityChangedPayload
//	@Failure		400	"invalid body"
//	@Failure		401	"not authenticated"
//	@Failure		409	"task is not pending — already running, completed, or absent"
//	@Failure		500	"server error"
//	@Tags			Pipeline queues
//	@Param			Authorization	header	string	true	"Insert your personal access token"	default(Bearer <personal access token>)
func PatchTaskPriority(c *gin.Context) {
	taskID := c.Param("task_id")
	if taskID == "" {
		c.String(http.StatusBadRequest, "missing task_id")
		return
	}

	var req patchPriorityRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.String(http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}

	user := session.User(c)
	if user == nil {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	actorEmail := c.GetHeader(authRequestEmailHeader)
	if actorEmail == "" {
		actorEmail = user.Email
	}
	if actorEmail == "" {
		// Fall back to login so the audit row is never empty — SOC 2
		// CC7.2 traceability requires an actor identifier.
		actorEmail = user.Login
	}

	oldPriority, err := server.Config.Services.Queue.UpdatePriority(taskID, req.Priority)
	if errors.Is(err, queue.ErrNotFound) {
		c.String(http.StatusConflict, "task is not pending — already running, completed, or absent")
		return
	}
	if err != nil {
		log.Error().Err(err).Str("task_id", taskID).Msg("priority: queue update failed")
		c.String(http.StatusInternalServerError, "queue update failed")
		return
	}

	// Persist the audit row regardless of whether downstream SSE delivery
	// works — the audit is the SOC 2 / ISO 27001 contract.
	_store := store.FromContext(c)
	audit := &model.PriorityAudit{
		ActorEmail:  actorEmail,
		TaskID:      taskID,
		OldPriority: oldPriority,
		NewPriority: req.Priority,
	}
	if auditErr := _store.PriorityAuditInsert(audit); auditErr != nil {
		// In-memory + DB priority changed but audit is missing — log
		// loudly for compliance investigation, but don't fail the
		// request: the user has done what they wanted, and reverting
		// the priority would create a worse state.
		log.Error().Err(auditErr).
			Str("task_id", taskID).Str("actor", actorEmail).
			Int64("old_priority", oldPriority).Int64("new_priority", req.Priority).
			Msg("priority: AUDIT WRITE FAILED — priority changed without audit row")
	}

	payload := taskPriorityChangedPayload{
		Type:        taskPriorityChangedEventType,
		TaskID:      taskID,
		OldPriority: oldPriority,
		NewPriority: req.Priority,
		ActorEmail:  actorEmail,
	}

	if data, err := json.Marshal(payload); err == nil {
		server.Config.Services.Pubsub.Publish(pubsub.Message{
			Labels: map[string]string{
				"private": "false",
			},
			Data: data,
		})
	} else {
		log.Error().Err(err).Msg("priority: failed to marshal SSE event")
	}

	c.JSON(http.StatusOK, payload)
}
