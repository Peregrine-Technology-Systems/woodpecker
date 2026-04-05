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
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog/log"

	"go.woodpecker-ci.org/woodpecker/v3/rpc"
	"go.woodpecker-ci.org/woodpecker/v3/server"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/pipeline"
	"go.woodpecker-ci.org/woodpecker/v3/server/plugin"
	"go.woodpecker-ci.org/woodpecker/v3/server/store"
)

// handleInit transitions a workflow from pending to running.
// Equivalent to RPC.Init() but without agent context.
func handleInit(c *gin.Context) {
	_store := store.FromContext(c)
	if _store == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "store not available"})
		return
	}
	workflowID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workflow id"})
		return
	}

	var state rpc.WorkflowState
	if err := c.ShouldBindJSON(&state); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	workflow, currentPipeline, repo, err := loadWorkflowContext(_store, workflowID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	if currentPipeline.Status == model.StatusPending {
		currentPipeline, err = pipeline.UpdateToStatusRunning(_store, *currentPipeline, state.Started)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		pipeline.EmitEvent(plugin.EventPipelineStarted, repo, currentPipeline, "")
	}

	if _, err := pipeline.UpdateWorkflowStatusToRunning(_store, *workflow, state); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	log.Debug().Int64("workflow", workflowID).Msg("status-api: workflow initialized")
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// handleUpdate updates a step's status within a workflow.
// Equivalent to RPC.Update() but without agent context.
func handleUpdate(c *gin.Context) {
	_store := store.FromContext(c)
	if _store == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "store not available"})
		return
	}
	workflowID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workflow id"})
		return
	}

	var state rpc.StepState
	if err := c.ShouldBindJSON(&state); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	_, currentPipeline, repo, err := loadWorkflowContext(_store, workflowID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	step, err := _store.StepByUUID(state.StepUUID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "step not found"})
		return
	}

	if err := pipeline.UpdateStepStatus(c, _store, step, state); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if state.Exited {
		pipeline.EmitEvent(plugin.EventStepCompleted, repo, currentPipeline, step.Name)
	}

	log.Debug().Int64("workflow", workflowID).Str("step", step.Name).Msg("status-api: step updated")
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// handleDone marks a workflow as complete and finalizes the pipeline if all
// workflows are done. Equivalent to RPC.Done() but without agent/queue context.
func handleDone(c *gin.Context) {
	_store := store.FromContext(c)
	if _store == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "store not available"})
		return
	}
	workflowID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workflow id"})
		return
	}

	var state rpc.WorkflowState
	if err := c.ShouldBindJSON(&state); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	workflow, currentPipeline, repo, err := loadWorkflowContext(_store, workflowID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	if _, err := pipeline.UpdateWorkflowStatusToDone(_store, *workflow, state); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Acknowledge in the queue so the server knows the task is done
	queue := server.Config.Services.Queue
	if queue != nil {
		queueID := strconv.FormatInt(workflowID, 10)
		if err := queue.Done(c, queueID, model.StatusSuccess); err != nil {
			log.Warn().Err(err).Int64("workflow", workflowID).Msg("status-api: queue done failed")
		}
	}

	// Check if all workflows are done — if so, finalize the pipeline
	if err := finalizePipelineIfDone(_store, currentPipeline, repo, state.Finished); err != nil {
		log.Warn().Err(err).Int64("pipeline", currentPipeline.ID).Msg("status-api: finalize failed")
	}

	log.Debug().Int64("workflow", workflowID).Msg("status-api: workflow done")
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
