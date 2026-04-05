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
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
)

// collectingHook records all events it receives.
type collectingHook struct {
	mu     sync.Mutex
	name   string
	events []PipelineEvent
	closed bool
}

func (h *collectingHook) Name() string { return h.name }

func (h *collectingHook) OnEvent(_ context.Context, event PipelineEvent) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, event)
	return nil
}

func (h *collectingHook) Close() error {
	h.closed = true
	return nil
}

func (h *collectingHook) getEvents() []PipelineEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]PipelineEvent{}, h.events...)
}

// errorHook always returns an error from OnEvent.
type errorHook struct {
	name string
}

func (h *errorHook) Name() string { return h.name }
func (h *errorHook) OnEvent(_ context.Context, _ PipelineEvent) error {
	return errors.New("hook error")
}
func (h *errorHook) Close() error { return nil }

// slowHook blocks until its context is canceled.
type slowHook struct {
	name string
}

func (h *slowHook) Name() string { return h.name }
func (h *slowHook) OnEvent(ctx context.Context, _ PipelineEvent) error {
	<-ctx.Done()
	return ctx.Err()
}
func (h *slowHook) Close() error { return nil }

func makeEvent(t EventType) PipelineEvent {
	return PipelineEvent{
		Type:       t,
		RepoID:     1,
		PipelineID: 42,
		Status:     model.StatusSuccess,
		Timestamp:  time.Now(),
	}
}

func TestEventBusPublishAndDeliver(t *testing.T) {
	bus := NewEventBus(context.Background())
	hook := &collectingHook{name: "test"}
	bus.addHook(hook)

	bus.Publish(makeEvent(EventPipelineCreated))
	bus.Publish(makeEvent(EventPipelineCompleted))

	// Allow delivery goroutine to process.
	assert.Eventually(t, func() bool {
		return len(hook.getEvents()) == 2
	}, time.Second, 10*time.Millisecond)

	events := hook.getEvents()
	assert.Equal(t, EventPipelineCreated, events[0].Type)
	assert.Equal(t, EventPipelineCompleted, events[1].Type)

	bus.Close()
}

func TestEventBusFanOut(t *testing.T) {
	bus := NewEventBus(context.Background())
	hook1 := &collectingHook{name: "h1"}
	hook2 := &collectingHook{name: "h2"}
	bus.addHook(hook1)
	bus.addHook(hook2)

	bus.Publish(makeEvent(EventPipelineStarted))

	assert.Eventually(t, func() bool {
		return len(hook1.getEvents()) == 1 && len(hook2.getEvents()) == 1
	}, time.Second, 10*time.Millisecond)

	bus.Close()
}

func TestEventBusCloseStopsDelivery(t *testing.T) {
	bus := NewEventBus(context.Background())
	hook := &collectingHook{name: "test"}
	bus.addHook(hook)

	bus.Close()

	// Publish after close should be silently dropped.
	bus.Publish(makeEvent(EventPipelineCreated))

	time.Sleep(50 * time.Millisecond)
	assert.Empty(t, hook.getEvents())
}

func TestEventBusDoubleClose(t *testing.T) {
	bus := NewEventBus(context.Background())
	bus.addHook(&collectingHook{name: "test"})

	bus.Close()
	bus.Close() // should not panic
}

func TestEventBusErrorHookDoesNotBlock(t *testing.T) {
	bus := NewEventBus(context.Background())
	errHook := &errorHook{name: "err"}
	goodHook := &collectingHook{name: "good"}
	bus.addHook(errHook)
	bus.addHook(goodHook)

	bus.Publish(makeEvent(EventPipelineFailed))

	assert.Eventually(t, func() bool {
		return len(goodHook.getEvents()) == 1
	}, time.Second, 10*time.Millisecond)

	bus.Close()
}

func TestEventBusSlowHookTimesOut(t *testing.T) {
	bus := NewEventBus(context.Background())
	slow := &slowHook{name: "slow"}
	fast := &collectingHook{name: "fast"}
	bus.addHook(slow)
	bus.addHook(fast)

	bus.Publish(makeEvent(EventPipelineCreated))

	// Fast hook should still receive the event.
	assert.Eventually(t, func() bool {
		return len(fast.getEvents()) == 1
	}, time.Second, 10*time.Millisecond)

	bus.Close()
}

func TestEventBusDropsOnFullBuffer(t *testing.T) {
	bus := NewEventBus(context.Background())

	// Use a slow hook so events pile up in the channel.
	blocker := &slowHook{name: "blocker"}
	bus.addHook(blocker)

	// Fill the buffer.
	for i := 0; i < eventBufferSize+10; i++ {
		bus.Publish(makeEvent(EventPipelineCreated))
	}

	// Should not deadlock or panic — just drop overflow events.
	bus.Close()
}

func TestEventBusAddHookAfterClose(t *testing.T) {
	bus := NewEventBus(context.Background())
	bus.Close()

	bus.addHook(&collectingHook{name: "late"})

	// No goroutine should be started; bus is closed.
	require.Empty(t, bus.subs)
}

func TestEventBusContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	bus := NewEventBus(ctx)
	hook := &collectingHook{name: "test"}
	bus.addHook(hook)

	bus.Publish(makeEvent(EventPipelineCreated))

	assert.Eventually(t, func() bool {
		return len(hook.getEvents()) == 1
	}, time.Second, 10*time.Millisecond)

	// Cancel the parent context — delivery goroutines should exit.
	cancel()
	time.Sleep(50 * time.Millisecond)

	bus.Close()
}

func TestEventBusNoSubscribers(t *testing.T) {
	bus := NewEventBus(context.Background())

	// Publish with no subscribers should not panic.
	bus.Publish(makeEvent(EventPipelineCreated))

	bus.Close()
}
