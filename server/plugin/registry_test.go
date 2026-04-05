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
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
)

// mock implementations for testing

type mockEventHook struct {
	name   string
	events []PipelineEvent
	closed bool
}

func (m *mockEventHook) Name() string { return m.name }
func (m *mockEventHook) OnEvent(_ context.Context, event PipelineEvent) error {
	m.events = append(m.events, event)
	return nil
}
func (m *mockEventHook) Close() error {
	m.closed = true
	return nil
}

type mockStatusHook struct {
	name       string
	registered bool
	closed     bool
}

func (m *mockStatusHook) Name() string                      { return m.name }
func (m *mockStatusHook) RegisterRoutes(_ *gin.RouterGroup) { m.registered = true }
func (m *mockStatusHook) Close() error {
	m.closed = true
	return nil
}

type mockDispatchHook struct {
	name   string
	closed bool
}

func (m *mockDispatchHook) Name() string { return m.name }
func (m *mockDispatchHook) Dispatch(_ context.Context, _ *model.Task) (bool, error) {
	return true, nil
}
func (m *mockDispatchHook) Close() error {
	m.closed = true
	return nil
}

func TestRegistryEventHooks(t *testing.T) {
	r := NewRegistry()

	hook1 := &mockEventHook{name: "hook1"}
	hook2 := &mockEventHook{name: "hook2"}

	r.RegisterEventHook(hook1)
	r.RegisterEventHook(hook2)

	hooks := r.EventHooks()
	assert.Len(t, hooks, 2)
	assert.Equal(t, "hook1", hooks[0].Name())
	assert.Equal(t, "hook2", hooks[1].Name())
}

func TestRegistryEventHooksReturnsCopy(t *testing.T) {
	r := NewRegistry()
	r.RegisterEventHook(&mockEventHook{name: "hook1"})

	hooks := r.EventHooks()
	hooks = append(hooks, &mockEventHook{name: "extra"})

	assert.Len(t, r.EventHooks(), 1, "original slice should not be modified")
}

func TestRegistryStatusHooks(t *testing.T) {
	r := NewRegistry()

	hook := &mockStatusHook{name: "status1"}
	r.RegisterStatusHook(hook)

	hooks := r.StatusHooks()
	assert.Len(t, hooks, 1)
	assert.Equal(t, "status1", hooks[0].Name())
}

func TestRegistryStatusHooksReturnsCopy(t *testing.T) {
	r := NewRegistry()
	r.RegisterStatusHook(&mockStatusHook{name: "s1"})

	hooks := r.StatusHooks()
	hooks = append(hooks, &mockStatusHook{name: "extra"})

	assert.Len(t, r.StatusHooks(), 1)
}

func TestRegistryDispatchHook(t *testing.T) {
	r := NewRegistry()

	assert.Nil(t, r.GetDispatchHook())

	hook := &mockDispatchHook{name: "dispatch1"}
	r.RegisterDispatchHook(hook)

	assert.Equal(t, "dispatch1", r.GetDispatchHook().Name())
}

func TestRegistryDispatchHookLastWins(t *testing.T) {
	r := NewRegistry()

	r.RegisterDispatchHook(&mockDispatchHook{name: "first"})
	r.RegisterDispatchHook(&mockDispatchHook{name: "second"})

	assert.Equal(t, "second", r.GetDispatchHook().Name())
}

func TestRegistryClose(t *testing.T) {
	r := NewRegistry()

	eh := &mockEventHook{name: "e1"}
	sh := &mockStatusHook{name: "s1"}
	dh := &mockDispatchHook{name: "d1"}

	r.RegisterEventHook(eh)
	r.RegisterStatusHook(sh)
	r.RegisterDispatchHook(dh)

	r.Close()

	assert.True(t, eh.closed)
	assert.True(t, sh.closed)
	assert.True(t, dh.closed)
}

func TestRegistryCloseWithoutDispatch(t *testing.T) {
	r := NewRegistry()
	r.RegisterEventHook(&mockEventHook{name: "e1"})

	r.Close() // should not panic with nil dispatchHook
}

func TestRegistrySetEventBusAddsExistingHooks(t *testing.T) {
	r := NewRegistry()
	hook := &mockEventHook{name: "pre-existing"}
	r.RegisterEventHook(hook)

	bus := NewEventBus(context.Background())
	defer bus.Close()

	r.SetEventBus(bus)

	assert.Equal(t, bus, r.GetEventBus())
	assert.Len(t, bus.subs, 1)
}

func TestRegistryRegisterEventHookAfterBus(t *testing.T) {
	r := NewRegistry()
	bus := NewEventBus(context.Background())
	defer bus.Close()

	r.SetEventBus(bus)
	r.RegisterEventHook(&mockEventHook{name: "late"})

	assert.Len(t, bus.subs, 1)
}

func TestRegistryGetEventBusNil(t *testing.T) {
	r := NewRegistry()
	assert.Nil(t, r.GetEventBus())
}

// errorCloseHook returns an error on Close.
type errorCloseEventHook struct {
	mockEventHook
}

func (m *errorCloseEventHook) Close() error { return errors.New("close failed") }

type errorCloseStatusHook struct {
	mockStatusHook
}

func (m *errorCloseStatusHook) Close() error { return errors.New("close failed") }

type errorCloseDispatchHook struct {
	mockDispatchHook
}

func (m *errorCloseDispatchHook) Close() error { return errors.New("close failed") }

func TestRegistryCloseWithErrors(t *testing.T) {
	r := NewRegistry()
	r.RegisterEventHook(&errorCloseEventHook{mockEventHook{name: "bad-event"}})
	r.RegisterStatusHook(&errorCloseStatusHook{mockStatusHook{name: "bad-status"}})
	r.RegisterDispatchHook(&errorCloseDispatchHook{mockDispatchHook{name: "bad-dispatch"}})

	// Should not panic despite close errors — just logs them.
	r.Close()
}

func TestRegistryCloseWithEventBus(t *testing.T) {
	r := NewRegistry()
	bus := NewEventBus(context.Background())
	r.SetEventBus(bus)
	r.RegisterEventHook(&mockEventHook{name: "e1"})

	r.Close() // should close bus and hooks
}
