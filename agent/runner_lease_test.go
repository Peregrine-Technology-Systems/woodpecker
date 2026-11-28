// Copyright 2026 Woodpecker Authors
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

package agent

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"

	"go.woodpecker-ci.org/woodpecker/v3/rpc"
	"go.woodpecker-ci.org/woodpecker/v3/shared/constant"
)

// #432: the lease loop renews at once when the transport says it reconnected,
// not only on its TaskTimeout/3 tick.

type renewingPeer struct {
	rpc.Peer
	mu        sync.Mutex
	extends   []string
	reconnect chan struct{}
	asked     int
}

func (p *renewingPeer) Extend(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.extends = append(p.extends, id)
	return nil
}

func (p *renewingPeer) Reconnected() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.asked++
	return p.reconnect
}

func (p *renewingPeer) subscriptions() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.asked
}

func (p *renewingPeer) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.extends)
}

func (p *renewingPeer) announce() {
	p.mu.Lock()
	defer p.mu.Unlock()
	close(p.reconnect)
	p.reconnect = make(chan struct{})
}

func TestLeaseRenewsAtOnceOnReconnect(t *testing.T) {
	p := &renewingPeer{reconnect: make(chan struct{})}
	r := &Runner{client: p}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.renewLease(ctx, "117400", log.Logger)
	assert.Eventually(t, func() bool { return p.subscriptions() == 1 }, time.Second, 5*time.Millisecond)

	p.announce()
	assert.Eventually(t, func() bool { return p.count() == 1 }, 2*time.Second, 10*time.Millisecond,
		"a reconnect must renew the lease within seconds, not at the next tick")
	p.announce()
	assert.Eventually(t, func() bool { return p.count() == 2 }, 2*time.Second, 10*time.Millisecond, "every reconnect")
}

// A transport that cannot tell (WS renews through its health report) keeps
// the tick only.
func TestLeaseWithoutReconnectSignalWaitsForTheTick(t *testing.T) {
	p := &countingPeer{}
	r := &Runner{client: p}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.renewLease(ctx, "117400", log.Logger)

	time.Sleep(200 * time.Millisecond)
	assert.Zero(t, p.n.Load())
}

type countingPeer struct {
	rpc.Peer
	n atomic.Int64
}

func (p *countingPeer) Extend(context.Context, string) error { p.n.Add(1); return nil }

type failingPeer struct {
	countingPeer
}

func (p *failingPeer) Extend(context.Context, string) error {
	p.n.Add(1)
	return errors.New("server unreachable")
}

// The tick still renews, and a failed renewal is logged and retried at the
// next one rather than ending the loop.
func TestLeaseRenewsOnTheTickAndSurvivesAFailure(t *testing.T) {
	orig := constant.TaskTimeout
	constant.TaskTimeout = 60 * time.Millisecond
	t.Cleanup(func() { constant.TaskTimeout = orig })
	p := &failingPeer{}
	r := &Runner{client: p}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.renewLease(ctx, "117400", log.Logger); close(done) }()

	assert.Eventually(t, func() bool { return p.n.Load() >= 2 }, 2*time.Second, 5*time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the lease loop must end with its workflow")
	}
}
