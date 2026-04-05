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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/plugin"
)

type recordingHook struct {
	events []plugin.PipelineEvent
}

func (h *recordingHook) Name() string { return "recorder" }
func (h *recordingHook) OnEvent(_ context.Context, event plugin.PipelineEvent) error {
	h.events = append(h.events, event)
	return nil
}
func (h *recordingHook) Close() error { return nil }

func TestName(t *testing.T) {
	d := New(plugin.NewRegistry())
	assert.Equal(t, "external-dispatch", d.Name())
}

func TestClose(t *testing.T) {
	d := New(plugin.NewRegistry())
	assert.NoError(t, d.Close())
}

func TestDispatchPublishesEvent(t *testing.T) {
	registry := plugin.NewRegistry()
	bus := plugin.NewEventBus(context.Background())
	registry.SetEventBus(bus)

	recorder := &recordingHook{}
	registry.RegisterEventHook(recorder)

	d := New(registry)
	task := &model.Task{ID: "42", PipelineID: 10, RepoID: 100}

	handled, err := d.Dispatch(context.Background(), task)
	require.NoError(t, err)
	assert.True(t, handled)

	// Give the async delivery a moment
	time.Sleep(50 * time.Millisecond)

	require.Len(t, recorder.events, 1)
	assert.Equal(t, plugin.EventTaskAvailable, recorder.events[0].Type)
	assert.Equal(t, int64(10), recorder.events[0].PipelineID)
	assert.Equal(t, int64(100), recorder.events[0].RepoID)
}

func TestDispatchWithoutBus(t *testing.T) {
	registry := plugin.NewRegistry()
	// No event bus set

	d := New(registry)
	task := &model.Task{ID: "42"}

	handled, err := d.Dispatch(context.Background(), task)
	require.NoError(t, err)
	assert.False(t, handled, "should not claim task without event bus")
}

func TestDispatchMultipleTasks(t *testing.T) {
	registry := plugin.NewRegistry()
	bus := plugin.NewEventBus(context.Background())
	registry.SetEventBus(bus)

	recorder := &recordingHook{}
	registry.RegisterEventHook(recorder)

	d := New(registry)

	for i := 1; i <= 3; i++ {
		task := &model.Task{ID: string(rune('0' + i)), PipelineID: int64(i * 10)}
		handled, err := d.Dispatch(context.Background(), task)
		require.NoError(t, err)
		assert.True(t, handled)
	}

	time.Sleep(50 * time.Millisecond)
	assert.Len(t, recorder.events, 3)
}
