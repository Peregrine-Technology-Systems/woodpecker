// Copyright 2026 Peregrine Technology Systems
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0

package api

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog/log"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/store"
)

// GetQueueAudit
//
//	@Summary		List queue priority audit entries
//	@Description	Returns priority-mutation audit rows in id-descending order. Cursor-paginate via the `before` query param (the id of the last row from the previous page).
//	@Router			/queue/audit [get]
//	@Produce		json
//	@Success		200	{array}	model.PriorityAudit
//	@Tags			Pipeline queues
//	@Param			limit			query	integer	false	"max rows to return (default 50, max 200)"
//	@Param			before			query	integer	false	"return rows with id strictly less than this value"
//	@Param			Authorization	header	string	true	"Insert your personal access token"	default(Bearer <personal access token>)
func GetQueueAudit(c *gin.Context) {
	_store := store.FromContext(c)

	opts := &model.PriorityAuditListOptions{}
	if v := c.Query("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			c.String(http.StatusBadRequest, "invalid limit")
			return
		}
		opts.Limit = n
	}
	if v := c.Query("before"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			c.String(http.StatusBadRequest, "invalid before")
			return
		}
		opts.BeforeID = n
	}

	rows, err := _store.PriorityAuditList(opts)
	if err != nil {
		log.Error().Err(err).Msg("queue audit: list failed")
		c.String(http.StatusInternalServerError, "audit list failed")
		return
	}

	c.JSON(http.StatusOK, rows)
}
