// Copyright 2026 Woodpecker Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package rpc

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"go.woodpecker-ci.org/woodpecker/v3/rpc/proto"
)

// fakeAuthClient implements proto.WoodpeckerAuthClient. It models the real
// server Auth RPC: it issues a fresh access token on each call but always
// returns the SAME agent_id it was handed — a JWT refresh never mints a new
// identity.
type fakeAuthClient struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (f *fakeAuthClient) Auth(_ context.Context, in *proto.AuthRequest, _ ...grpc.CallOption) (*proto.AuthResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.calls++
	return &proto.AuthResponse{
		AgentId:     in.AgentId, // server preserves the agent_id across refreshes
		AccessToken: fmt.Sprintf("jwt-%d", f.calls),
	}, nil
}

func (f *fakeAuthClient) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// TestAuthInterceptorRefreshesJWTInPlaceKeepingAgentID is the #252 regression
// guard. The gRPC auth interceptor refreshes the access token IN PLACE — a new
// JWT, the same agent_id, the same connection. This is precisely why the old
// #116 "exit at 75% of token lifetime → systemd restart → fresh registration"
// mechanism was redundant: token expiry is already prevented here without
// recycling the agent_id (the recycle that stranded straddler workflows in
// #243/#246/#248). If this property ever breaks, the case for deleting #116
// breaks with it.
func TestAuthInterceptorRefreshesJWTInPlaceKeepingAgentID(t *testing.T) {
	const agentID int64 = 23000
	authClient := &AuthClient{client: &fakeAuthClient{}, agentToken: "durable-secret", agentID: agentID}
	interceptor := &AuthInterceptor{authClient: authClient}

	require.NoError(t, interceptor.refreshToken(context.Background()))
	first := interceptor.accessToken
	assert.Equal(t, "jwt-1", first)
	assert.Equal(t, agentID, authClient.agentID, "agent_id must be unchanged after the first refresh")

	require.NoError(t, interceptor.refreshToken(context.Background()))
	second := interceptor.accessToken
	assert.Equal(t, "jwt-2", second)
	assert.NotEqual(t, first, second, "each refresh swaps in a new access token in place")
	assert.Equal(t, agentID, authClient.agentID, "agent_id must STILL be unchanged after a second refresh (#252)")
}

// TestAuthInterceptorScheduleRefreshesOnInterval proves the in-place refresh is
// driven automatically on a timer (no task activity required), so a long-running
// or idle agent never reaches token expiry — the protection #116 was meant to
// provide already exists here.
func TestAuthInterceptorScheduleRefreshesOnInterval(t *testing.T) {
	fake := &fakeAuthClient{}
	authClient := &AuthClient{client: fake, agentToken: "durable-secret", agentID: 42}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	interceptor, err := NewAuthInterceptor(ctx, authClient, 20*time.Millisecond)
	require.NoError(t, err)
	require.NotNil(t, interceptor)

	assert.Eventually(t, func() bool { return fake.callCount() >= 3 },
		2*time.Second, 10*time.Millisecond, "interceptor must refresh the token on its schedule")
	assert.Equal(t, int64(42), authClient.agentID, "scheduled refreshes never change the agent_id")
}

// TestAuthInterceptorAttachesCurrentToken confirms the refreshed token is the
// one attached to outgoing RPCs.
func TestAuthInterceptorAttachesCurrentToken(t *testing.T) {
	authClient := &AuthClient{client: &fakeAuthClient{}, agentToken: "s", agentID: 1}
	interceptor := &AuthInterceptor{authClient: authClient}
	require.NoError(t, interceptor.refreshToken(context.Background()))

	ctx := interceptor.attachToken(context.Background())
	md, ok := metadata.FromOutgoingContext(ctx)
	require.True(t, ok)
	assert.Equal(t, []string{"jwt-1"}, md.Get("token"))
}
