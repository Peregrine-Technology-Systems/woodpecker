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
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"go.woodpecker-ci.org/woodpecker/v3/server"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/plugin"
)

type collectingHook struct {
	mu     sync.Mutex
	name   string
	events []plugin.PipelineEvent
}

func (h *collectingHook) Name() string { return h.name }

func (h *collectingHook) OnEvent(_ context.Context, event plugin.PipelineEvent) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, event)
	return nil
}

func (h *collectingHook) Close() error { return nil }

func (h *collectingHook) getEvents() []plugin.PipelineEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]plugin.PipelineEvent{}, h.events...)
}

func TestEmitEventWithPlugins(t *testing.T) {
	registry := plugin.NewRegistry()
	bus := plugin.NewEventBus(context.Background())
	defer bus.Close()
	registry.SetEventBus(bus)

	hook := &collectingHook{name: "test"}
	registry.RegisterEventHook(hook)

	server.Config.Services.Plugins = registry
	defer func() { server.Config.Services.Plugins = nil }()

	repo := &model.Repo{ID: 1, FullName: "owner/repo"}
	p := &model.Pipeline{
		ID:      42,
		Number:  7,
		Status:  model.StatusSuccess,
		Ref:     "refs/heads/main",
		Branch:  "main",
		Author:  "test-user",
		Message: "test commit",
	}

	EmitEvent(plugin.EventPipelineCreated, repo, p, "")

	assert.Eventually(t, func() bool {
		return len(hook.getEvents()) == 1
	}, time.Second, 10*time.Millisecond)

	event := hook.getEvents()[0]
	assert.Equal(t, plugin.EventPipelineCreated, event.Type)
	assert.Equal(t, int64(1), event.RepoID)
	assert.Equal(t, "owner/repo", event.RepoName)
	assert.Equal(t, int64(42), event.PipelineID)
	assert.Equal(t, int64(7), event.Number)
	assert.Equal(t, model.StatusSuccess, event.Status)
	assert.Equal(t, "main", event.Branch)
	assert.Equal(t, "test-user", event.Author)
	assert.Empty(t, event.StepName)
}

func TestEmitEventWithStepName(t *testing.T) {
	registry := plugin.NewRegistry()
	bus := plugin.NewEventBus(context.Background())
	defer bus.Close()
	registry.SetEventBus(bus)

	hook := &collectingHook{name: "test"}
	registry.RegisterEventHook(hook)

	server.Config.Services.Plugins = registry
	defer func() { server.Config.Services.Plugins = nil }()

	repo := &model.Repo{ID: 1, FullName: "owner/repo"}
	p := &model.Pipeline{ID: 42, Number: 7, Status: model.StatusSuccess}

	EmitEvent(plugin.EventStepCompleted, repo, p, "build")

	assert.Eventually(t, func() bool {
		return len(hook.getEvents()) == 1
	}, time.Second, 10*time.Millisecond)

	assert.Equal(t, "build", hook.getEvents()[0].StepName)
}

func TestEmitEventNilRegistry(t *testing.T) {
	server.Config.Services.Plugins = nil

	repo := &model.Repo{ID: 1, FullName: "owner/repo"}
	p := &model.Pipeline{ID: 42, Number: 7}

	// Should not panic.
	EmitEvent(plugin.EventPipelineCreated, repo, p, "")
}

func TestEmitEventNilBus(t *testing.T) {
	registry := plugin.NewRegistry()
	// No bus set.
	server.Config.Services.Plugins = registry
	defer func() { server.Config.Services.Plugins = nil }()

	repo := &model.Repo{ID: 1, FullName: "owner/repo"}
	p := &model.Pipeline{ID: 42, Number: 7}

	// Should not panic.
	EmitEvent(plugin.EventPipelineCreated, repo, p, "")
}

func TestEmitEventAllTypes(t *testing.T) {
	registry := plugin.NewRegistry()
	bus := plugin.NewEventBus(context.Background())
	defer bus.Close()
	registry.SetEventBus(bus)

	hook := &collectingHook{name: "test"}
	registry.RegisterEventHook(hook)

	server.Config.Services.Plugins = registry
	defer func() { server.Config.Services.Plugins = nil }()

	repo := &model.Repo{ID: 1, FullName: "owner/repo"}
	p := &model.Pipeline{ID: 42, Number: 7, Status: model.StatusRunning}

	allTypes := []plugin.EventType{
		plugin.EventPipelineCreated,
		plugin.EventPipelinePending,
		plugin.EventPipelineStarted,
		plugin.EventPipelineCompleted,
		plugin.EventPipelineFailed,
		plugin.EventPipelineKilled,
		plugin.EventStepCompleted,
	}

	for _, et := range allTypes {
		EmitEvent(et, repo, p, "")
	}

	assert.Eventually(t, func() bool {
		return len(hook.getEvents()) == len(allTypes)
	}, time.Second, 10*time.Millisecond)

	events := hook.getEvents()
	for i, et := range allTypes {
		assert.Equal(t, et, events[i].Type, "event %d", i)
	}
}
