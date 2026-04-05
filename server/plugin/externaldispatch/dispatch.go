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

package externaldispatch

import (
	"context"
	"time"

	"github.com/rs/zerolog/log"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/plugin"
)

// Dispatcher implements plugin.DispatchHook by claiming all pending tasks
// for external dispatch. It publishes a task.available event to the event
// bus so that external systems (e.g. scaler → WebSocket → agent) can pick
// up the work. Agents report back via the Status API plugin.
type Dispatcher struct {
	registry *plugin.Registry
}

// New creates a Dispatcher that publishes task events to the given registry's bus.
func New(registry *plugin.Registry) *Dispatcher {
	return &Dispatcher{registry: registry}
}

func (d *Dispatcher) Name() string { return "external-dispatch" }

// Dispatch claims a task for external handling and emits a task.available event.
// The queue moves the task to running; external agents complete it via Status API.
func (d *Dispatcher) Dispatch(_ context.Context, task *model.Task) (bool, error) {
	bus := d.registry.GetEventBus()
	if bus == nil {
		log.Warn().Str("task", task.ID).Msg("external-dispatch: no event bus, skipping")
		return false, nil
	}

	bus.Publish(plugin.PipelineEvent{
		Type:       plugin.EventTaskAvailable,
		PipelineID: task.PipelineID,
		RepoID:     task.RepoID,
		Timestamp:  time.Now(),
	})

	log.Debug().Str("task", task.ID).Msg("external-dispatch: task claimed")
	return true, nil
}

func (d *Dispatcher) Close() error { return nil }
