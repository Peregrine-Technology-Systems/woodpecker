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
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

const (
	eventBufferSize  = 1024
	hookDeadline     = 5 * time.Second
	shutdownDeadline = 10 * time.Second
)

// EventBus delivers PipelineEvents to registered EventHooks.
// Each hook gets its own delivery goroutine with a buffered channel.
// Publish is non-blocking: if a hook's buffer is full, the event is dropped.
type EventBus struct {
	mu       sync.Mutex
	subs     []*subscription
	closed   bool
	cancelFn context.CancelFunc
	ctx      context.Context
}

type subscription struct {
	hook EventHook
	ch   chan PipelineEvent
	done chan struct{}
}

// NewEventBus creates a running event bus.
func NewEventBus(ctx context.Context) *EventBus {
	ctx, cancel := context.WithCancel(ctx)
	return &EventBus{
		ctx:      ctx,
		cancelFn: cancel,
	}
}

// addHook starts a delivery goroutine for the given hook.
func (b *EventBus) addHook(hook EventHook) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return
	}

	sub := &subscription{
		hook: hook,
		ch:   make(chan PipelineEvent, eventBufferSize),
		done: make(chan struct{}),
	}
	b.subs = append(b.subs, sub)
	go b.deliver(sub)
}

// deliver drains a subscription's channel, calling OnEvent for each.
func (b *EventBus) deliver(sub *subscription) {
	defer close(sub.done)

	for {
		select {
		case event, ok := <-sub.ch:
			if !ok {
				return
			}
			ctx, cancel := context.WithTimeout(b.ctx, hookDeadline)
			if err := sub.hook.OnEvent(ctx, event); err != nil {
				log.Warn().
					Err(err).
					Str("plugin", sub.hook.Name()).
					Str("event", string(event.Type)).
					Msg("event hook delivery failed")
			}
			cancel()
		case <-b.ctx.Done():
			return
		}
	}
}

// Publish sends an event to all subscribed hooks.
// Non-blocking: drops the event for any hook whose buffer is full.
func (b *EventBus) Publish(event PipelineEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return
	}

	for _, sub := range b.subs {
		select {
		case sub.ch <- event:
		default:
			log.Warn().
				Str("plugin", sub.hook.Name()).
				Str("event", string(event.Type)).
				Msg("event bus: hook buffer full, dropping event")
		}
	}
}

// Close stops the event bus and waits for all delivery goroutines to drain.
func (b *EventBus) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	b.cancelFn()

	subs := append([]*subscription{}, b.subs...)
	for _, sub := range subs {
		close(sub.ch)
	}
	b.mu.Unlock()

	// Wait for delivery goroutines with a deadline.
	done := make(chan struct{})
	go func() {
		for _, sub := range subs {
			<-sub.done
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(shutdownDeadline):
		log.Warn().Msg("event bus: shutdown deadline exceeded, some hooks may not have drained")
	}
}
