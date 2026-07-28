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

package main

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/urfave/cli/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	"go.woodpecker-ci.org/woodpecker/v3/rpc/proto"
	"go.woodpecker-ci.org/woodpecker/v3/server"
	woodpeckerGrpcServer "go.woodpecker-ci.org/woodpecker/v3/server/rpc"
	"go.woodpecker-ci.org/woodpecker/v3/server/store"
)

func runGrpcServer(ctx context.Context, c *cli.Command, _store store.Store) error {
	lis, err := net.Listen("tcp", c.String("grpc-addr"))
	if err != nil {
		return fmt.Errorf("failed to listen on grpc-addr: %w", err)
	}

	jwtSecret := c.String("grpc-secret")
	jwtManager := woodpeckerGrpcServer.NewJWTManager(jwtSecret)

	authorizer := woodpeckerGrpcServer.NewAuthorizer(jwtManager)
	grpcServer := grpc.NewServer(
		grpc.StreamInterceptor(authorizer.StreamInterceptor),
		grpc.UnaryInterceptor(authorizer.UnaryInterceptor),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime: c.Duration("keepalive-min-time"),
			// Tolerate keepalive pings from a connected-but-idle agent (no active
			// stream). Without this the server GoAway'd idle gRPC agents with
			// ENHANCE_YOUR_CALM/too_many_pings and their workflow-RPC channel wedged
			// while the heartbeat stayed alive — claim-but-can't-report (woodpecker#312).
			PermitWithoutStream: true,
		}),
	)

	// Use the shared RPC peer (created in setup.go, shared with WS transport)
	rpcPeer := server.Config.Services.WSAgentRPC.(*woodpeckerGrpcServer.RPC)
	woodpeckerServer := woodpeckerGrpcServer.NewWoodpeckerServer(rpcPeer)
	proto.RegisterWoodpeckerServer(grpcServer, woodpeckerServer)

	woodpeckerAuthServer := woodpeckerGrpcServer.NewWoodpeckerAuthServer(
		jwtManager,
		server.Config.Server.AgentToken,
		_store,
	)
	proto.RegisterWoodpeckerAuthServer(grpcServer, woodpeckerAuthServer)

	grpcCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	go func() {
		<-grpcCtx.Done()
		if grpcServer == nil {
			return
		}
		// infra#5208/woodpecker#335: grpc.Server.GracefulStop() has no timeout of
		// its own -- it blocks until every currently-connected agent's long-poll
		// Next() stream closes naturally, however long that takes. With a large
		// connected fleet, that turned a deploy restart's SIGTERM into a ~10min
		// hang (systemd eventually SIGKILLs it), during which the webhook
		// receiver on the same process is also down. Bound it: give agents
		// shutdownTimeout to disconnect gracefully, then hard-stop so the
		// process actually exits promptly and predictably.
		log.Info().Msg("terminating grpc service gracefully")
		stopped := make(chan struct{})
		go func() {
			grpcServer.GracefulStop()
			close(stopped)
		}()
		select {
		case <-stopped:
			log.Info().Msg("grpc service stopped gracefully")
		case <-time.After(shutdownTimeout):
			log.Warn().Dur("timeout", shutdownTimeout).
				Msg("grpc graceful stop timed out — forcing hard stop so shutdown isn't unbounded")
			grpcServer.Stop()
		}
	}()

	if err := grpcServer.Serve(lis); err != nil {
		// signal that we don't have to stop the server gracefully anymore
		grpcServer = nil

		// wrap the error so we know where it did come from
		return fmt.Errorf("grpc server failed: %w", err)
	}

	return nil
}
