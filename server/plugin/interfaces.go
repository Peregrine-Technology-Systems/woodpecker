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

package plugin

import (
	"context"
	"time"

	"github.com/gin-gonic/gin"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/store"
)

// EventType identifies the kind of pipeline lifecycle event.
type EventType string

const (
	EventPipelineCreated   EventType = "pipeline.created"
	EventPipelinePending   EventType = "pipeline.pending"
	EventPipelineStarted   EventType = "pipeline.started"
	EventPipelineCompleted EventType = "pipeline.completed"
	EventPipelineFailed    EventType = "pipeline.failed"
	EventPipelineKilled    EventType = "pipeline.killed"
	EventStepCompleted     EventType = "step.completed"
)

// PipelineEvent carries data about a pipeline lifecycle transition.
type PipelineEvent struct {
	Type       EventType         `json:"type"`
	RepoID     int64             `json:"repo_id"`
	RepoName   string            `json:"repo_name"`
	PipelineID int64             `json:"pipeline_id"`
	Number     int64             `json:"number"`
	Status     model.StatusValue `json:"status"`
	Ref        string            `json:"ref"`
	Branch     string            `json:"branch"`
	Commit     string            `json:"commit"`
	Author     string            `json:"author"`
	Message    string            `json:"message"`
	Event      string            `json:"event"`
	StepName   string            `json:"step_name,omitempty"`
	Timestamp  time.Time         `json:"timestamp"`
}

// EventHook receives pipeline lifecycle events via fan-out.
// Multiple EventHooks can be registered; each receives all events.
type EventHook interface {
	Name() string
	OnEvent(ctx context.Context, event PipelineEvent) error
	Close() error
}

// StatusHook registers REST endpoints for external status updates.
// Each StatusHook owns a sub-path under /api/plugins/<name>/.
type StatusHook interface {
	Name() string
	RegisterRoutes(group *gin.RouterGroup, store store.Store)
	Close() error
}

// DispatchHook intercepts task assignment in the queue.
// Only one DispatchHook is active at a time (last registered wins).
// Returning handled=true removes the task from the FIFO queue.
type DispatchHook interface {
	Name() string
	Dispatch(ctx context.Context, task *model.Task) (handled bool, err error)
	Close() error
}
