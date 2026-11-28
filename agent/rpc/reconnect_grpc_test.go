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
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"

	"go.woodpecker-ci.org/woodpecker/v3/rpc/proto"
)

// #432: a gRPC agent learns that its connection came back, so it can renew its
// workflow leases at once instead of on its next TaskTimeout/3 tick. After a
// server restart that is what keeps restored work alive on a short lease.

// fakeStates plays a scripted sequence of connectivity states.
type fakeStates struct {
	states   []connectivity.State
	i        atomic.Int32
	connects atomic.Int32
}

func (f *fakeStates) Connect() { f.connects.Add(1) }

func (f *fakeStates) GetState() connectivity.State {
	i := int(f.i.Load())
	if i >= len(f.states) {
		return f.states[len(f.states)-1]
	}
	return f.states[i]
}

func (f *fakeStates) WaitForStateChange(ctx context.Context, _ connectivity.State) bool {
	if int(f.i.Load())+1 >= len(f.states) {
		<-ctx.Done()
		return false
	}
	f.i.Add(1)
	return true
}

func closedWithin(ch <-chan struct{}, d time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(d):
		return false
	}
}

func TestReconnects_FireOnReturnToReady(t *testing.T) {
	r := newReconnects()
	first := r.wait()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchReconnects(ctx, &fakeStates{states: []connectivity.State{
		connectivity.Ready, connectivity.TransientFailure, connectivity.Connecting, connectivity.Ready,
	}}, r)

	assert.True(t, closedWithin(first, time.Second), "a return to READY is announced")
	assert.False(t, closedWithin(r.wait(), 100*time.Millisecond), "and announced once")
}

func TestReconnects_SilentWhileTheConnectionIsDown(t *testing.T) {
	r := newReconnects()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchReconnects(ctx, &fakeStates{states: []connectivity.State{
		connectivity.Ready, connectivity.TransientFailure, connectivity.Connecting,
	}}, r)

	assert.False(t, closedWithin(r.wait(), 200*time.Millisecond))
}

// A lost connection goes IDLE and would wait there for the next call.
func TestReconnects_AnIdleConnectionIsAskedToConnect(t *testing.T) {
	r := newReconnects()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &fakeStates{states: []connectivity.State{connectivity.Ready, connectivity.Idle, connectivity.Connecting, connectivity.Ready}}
	go watchReconnects(ctx, f, r)

	assert.True(t, closedWithin(r.wait(), time.Second))
	assert.Equal(t, int32(1), f.connects.Load())
}

// pingServer answers only what a reconnect needs to be seen.
type pingServer struct {
	proto.UnimplementedWoodpeckerServer
}

func serveOn(t *testing.T, addr string) (*grpc.Server, string) {
	t.Helper()
	var lis net.Listener
	var err error
	require.Eventually(t, func() bool { lis, err = net.Listen("tcp", addr); return err == nil }, 5*time.Second, 50*time.Millisecond)
	gs := grpc.NewServer()
	proto.RegisterWoodpeckerServer(gs, &pingServer{})
	go func() { _ = gs.Serve(lis) }()
	return gs, lis.Addr().String()
}

// End to end over TCP: the server goes away and a new one comes up on the same
// address, as on a restart. The real client must announce the reconnect.
func TestGrpcClient_AnnouncesReconnectAfterServerRestart(t *testing.T) {
	first, addr := serveOn(t, "127.0.0.1:0")
	conn, err := grpc.NewClient("passthrough:///"+addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c, ok := NewGrpcClient(ctx, conn).(*client)
	require.True(t, ok)
	t.Cleanup(func() { _ = c.Close() })
	conn.Connect()
	require.Eventually(t, func() bool { return conn.GetState() == connectivity.Ready }, 5*time.Second, 10*time.Millisecond)
	announced := c.Reconnected()

	first.Stop()
	second, _ := serveOn(t, addr)
	t.Cleanup(second.Stop)

	assert.True(t, closedWithin(announced, 15*time.Second), "the client must announce it is connected again")
}
