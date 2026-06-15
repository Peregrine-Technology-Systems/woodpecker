// Copyright 2023 Woodpecker Authors
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
	"sync/atomic"
	"time"

	"google.golang.org/grpc"

	"go.woodpecker-ci.org/woodpecker/v3/rpc/proto"
)

const authClientTimeout = time.Second * 5

type AuthClient struct {
	client     proto.WoodpeckerAuthClient
	conn       *grpc.ClientConn
	agentToken string
	// agentID is written by the scheduled refresh goroutine (Auth) and read on
	// every outgoing RPC, so it is atomic to avoid a data race (#297).
	agentID atomic.Int64
}

func NewAuthGrpcClient(conn *grpc.ClientConn, agentToken string, agentID int64) *AuthClient {
	client := new(AuthClient)
	client.client = proto.NewWoodpeckerAuthClient(conn)
	client.conn = conn
	client.agentToken = agentToken
	client.agentID.Store(agentID)
	return client
}

func (c *AuthClient) Auth(ctx context.Context) (string, int64, error) {
	ctx, cancel := context.WithTimeout(ctx, authClientTimeout)
	defer cancel()

	req := &proto.AuthRequest{
		AgentToken: c.agentToken,
		AgentId:    c.agentID.Load(),
	}

	res, err := c.client.Auth(ctx, req)
	if err != nil {
		return "", -1, err
	}

	c.agentID.Store(res.GetAgentId())

	return res.GetAccessToken(), c.agentID.Load(), nil
}
