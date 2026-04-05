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

package pipeline

import (
	"time"

	"github.com/rs/zerolog/log"

	"go.woodpecker-ci.org/woodpecker/v3/server"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/plugin"
)

// EmitEvent publishes a pipeline lifecycle event to all registered EventHooks.
// Safe to call when no plugins are configured — returns immediately.
func EmitEvent(eventType plugin.EventType, repo *model.Repo, p *model.Pipeline, stepName string) {
	registry := server.Config.Services.Plugins
	if registry == nil {
		return
	}

	bus := registry.GetEventBus()
	if bus == nil {
		return
	}

	event := plugin.PipelineEvent{
		Type:       eventType,
		RepoID:     repo.ID,
		RepoName:   repo.FullName,
		PipelineID: p.ID,
		Number:     p.Number,
		Status:     p.Status,
		Ref:        p.Ref,
		Branch:     p.Branch,
		Commit:     p.Commit,
		Author:     p.Author,
		Message:    p.Message,
		Event:      string(p.Event),
		StepName:   stepName,
		Timestamp:  time.Now(),
	}

	bus.Publish(event)
	log.Debug().
		Str("event", string(eventType)).
		Str("repo", repo.FullName).
		Int64("pipeline", p.Number).
		Msg("plugin event emitted")
}
