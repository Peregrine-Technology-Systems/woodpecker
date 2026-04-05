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
	"sync"

	"github.com/rs/zerolog/log"
)

// Registry holds all registered plugin hooks.
type Registry struct {
	mu           sync.RWMutex
	eventHooks   []EventHook
	statusHooks  []StatusHook
	dispatchHook DispatchHook
	eventBus     *EventBus
}

// NewRegistry creates an empty plugin registry.
func NewRegistry() *Registry {
	return &Registry{}
}

// RegisterEventHook adds an event hook.
// Multiple hooks are supported; each receives all events.
func (r *Registry) RegisterEventHook(hook EventHook) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.eventHooks = append(r.eventHooks, hook)
	log.Info().Str("plugin", hook.Name()).Msg("registered event hook")

	if r.eventBus != nil {
		r.eventBus.addHook(hook)
	}
}

// RegisterStatusHook adds a status hook.
func (r *Registry) RegisterStatusHook(hook StatusHook) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.statusHooks = append(r.statusHooks, hook)
	log.Info().Str("plugin", hook.Name()).Msg("registered status hook")
}

// RegisterDispatchHook sets the dispatch hook.
// Only one is active; last registered wins.
func (r *Registry) RegisterDispatchHook(hook DispatchHook) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.dispatchHook != nil {
		log.Warn().
			Str("old", r.dispatchHook.Name()).
			Str("new", hook.Name()).
			Msg("replacing dispatch hook")
	}
	r.dispatchHook = hook
	log.Info().Str("plugin", hook.Name()).Msg("registered dispatch hook")
}

// EventHooks returns all registered event hooks.
func (r *Registry) EventHooks() []EventHook {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return append([]EventHook{}, r.eventHooks...)
}

// StatusHooks returns all registered status hooks.
func (r *Registry) StatusHooks() []StatusHook {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return append([]StatusHook{}, r.statusHooks...)
}

// DispatchHook returns the active dispatch hook, or nil.
func (r *Registry) GetDispatchHook() DispatchHook {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.dispatchHook
}

// SetEventBus attaches the event bus to the registry.
// Any already-registered event hooks are added to the bus.
func (r *Registry) SetEventBus(bus *EventBus) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.eventBus = bus
	for _, hook := range r.eventHooks {
		bus.addHook(hook)
	}
}

// GetEventBus returns the attached event bus, or nil.
func (r *Registry) GetEventBus() *EventBus {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.eventBus
}

// Close shuts down all registered hooks.
func (r *Registry) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.eventBus != nil {
		r.eventBus.Close()
	}

	for _, hook := range r.eventHooks {
		if err := hook.Close(); err != nil {
			log.Error().Err(err).Str("plugin", hook.Name()).Msg("error closing event hook")
		}
	}
	for _, hook := range r.statusHooks {
		if err := hook.Close(); err != nil {
			log.Error().Err(err).Str("plugin", hook.Name()).Msg("error closing status hook")
		}
	}
	if r.dispatchHook != nil {
		if err := r.dispatchHook.Close(); err != nil {
			log.Error().Err(err).Str("plugin", r.dispatchHook.Name()).Msg("error closing dispatch hook")
		}
	}
}
