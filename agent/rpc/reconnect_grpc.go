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

package rpc

import (
	"context"
	"sync"

	"google.golang.org/grpc/connectivity"
)

// [pts] #432: the gRPC agent learns when its connection comes back. gRPC
// reconnects underneath the client, so nothing else tells the agent that the
// server it was talking to went away. After a server restart the server knows
// a restored task only by a short first lease (#427), and the agent's own lease
// loop renews only every TaskTimeout/3, so the agent renews on this signal.

// reconnects broadcasts each return to READY: wait returns a channel that is
// closed at the next one.
type reconnects struct {
	mu sync.Mutex
	ch chan struct{}
}

func newReconnects() *reconnects { return &reconnects{ch: make(chan struct{})} }

func (r *reconnects) wait() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ch
}

func (r *reconnects) fire() {
	r.mu.Lock()
	defer r.mu.Unlock()
	close(r.ch)
	r.ch = make(chan struct{})
}

// stateConn is the part of *grpc.ClientConn the watcher reads.
type stateConn interface {
	GetState() connectivity.State
	WaitForStateChange(ctx context.Context, s connectivity.State) bool
	Connect()
}

// watchReconnects fires r each time conn moves into READY from another state,
// until ctx ends. A lost connection goes IDLE and stays there until the next
// call, which for an agent between health reports can be a while, so the
// watcher asks it to connect at once.
func watchReconnects(ctx context.Context, conn stateConn, r *reconnects) {
	last := conn.GetState()
	for conn.WaitForStateChange(ctx, last) {
		now := conn.GetState()
		switch {
		case now == connectivity.Ready && last != connectivity.Ready:
			r.fire()
		case now == connectivity.Idle:
			conn.Connect()
		}
		last = now
	}
}

// Reconnected returns a channel closed when the connection next comes back.
func (c *client) Reconnected() <-chan struct{} { return c.reconnects.wait() }
