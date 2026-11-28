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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	grpcMetadata "google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"go.woodpecker-ci.org/woodpecker/v3/rpc"
	"go.woodpecker-ci.org/woodpecker/v3/rpc/proto"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	grpcserver "go.woodpecker-ci.org/woodpecker/v3/server/rpc"
	store_mocks "go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
)

// #397, end to end: the REAL gRPC agent client against the REAL gRPC server
// and RPC, over an in-memory connection. Only the store is a fake. Before #397
// the client returned the raw FailedPrecondition, so the tracer could not tell
// the server had abandoned the workflow and kept running it.

// grpcClientFor serves srv over an in-memory listener and returns the real
// agent client dialled to it. Calls carry the agent id, as the auth interceptor
// would in production.
func grpcClientFor(t *testing.T, srv proto.WoodpeckerServer) rpc.Peer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c := NewGrpcClient(ctx, bufconnClientConn(t, srv))
	t.Cleanup(func() { _ = c.(*client).Close() })
	return c
}

func bufconnClientConn(t *testing.T, srv proto.WoodpeckerServer) *grpc.ClientConn {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer(grpc.MaxRecvMsgSize(4 << 20))
	proto.RegisterWoodpeckerServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(cancelAfterCall),
	)
	require.NoError(t, err)
	return conn
}

func agentCallCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wsTestDeadline)
	t.Cleanup(cancel)
	return grpcMetadata.AppendToOutgoingContext(ctx, "agent_id", "61493")
}

type grpcTerminalFixture struct {
	peer      rpc.Peer
	stepReads atomic.Int64
}

func newGRPCTerminalFixture(t *testing.T) *grpcTerminalFixture {
	t.Helper()
	f := &grpcTerminalFixture{}
	s := store_mocks.NewMockStore(t)
	s.On("AgentFind", int64(61493)).Return(&model.Agent{ID: 61493, OwnerID: model.IDNotSet, OrgID: model.IDNotSet}, nil).Maybe()
	s.On("WorkflowLoad", mock.Anything).Return(&model.Workflow{ID: 117374, PipelineID: 100}, nil).Maybe()
	s.On("GetPipeline", mock.Anything).Return(&model.Pipeline{ID: 100, RepoID: 9, Status: model.StatusKilled}, nil).Maybe()
	s.On("GetRepo", mock.Anything).Return(&model.Repo{ID: 9, FullName: "org/infra"}, nil).Maybe()
	s.On("StepByUUID", killedStepUUID).Return(&model.Step{
		ID: 1, UUID: killedStepUUID, PipelineID: 100, State: model.StatusKilled,
	}, nil).Run(func(mock.Arguments) { f.stepReads.Add(1) }).Maybe()
	s.On("StepByUUID", successStepUUID).Return(&model.Step{
		ID: 2, UUID: successStepUUID, PipelineID: 100, State: model.StatusSuccess,
	}, nil).Maybe()
	f.peer = grpcClientFor(t, grpcserver.NewWoodpeckerServer(grpcserver.NewRPCForTesting(nil, s)))
	return f
}

func TestGRPCClient_TerminalRejectionOfKilledStep_IsAbandonment(t *testing.T) {
	f := newGRPCTerminalFixture(t)

	err := f.peer.Update(agentCallCtx(t), "117374", rpc.StepState{StepUUID: killedStepUUID, Exited: true})

	require.ErrorIs(t, err, rpc.ErrStepTerminal, "a terminal-state rejection must come back typed")
	assert.ErrorIs(t, err, rpc.ErrStepAbandoned, "a killed step means the server abandoned the workflow")
	assert.Equal(t, int64(1), f.stepReads.Load(), "a permanent rejection is sent once")
}

// A re-sent final update finds the step already recorded: permanent, but not
// abandonment, so the agent must not stop healthy work.
func TestGRPCClient_TerminalRejectionOfRecordedResult_IsNotAbandonment(t *testing.T) {
	f := newGRPCTerminalFixture(t)

	err := f.peer.Update(agentCallCtx(t), "117374", rpc.StepState{StepUUID: successStepUUID, Exited: true})

	require.ErrorIs(t, err, rpc.ErrStepTerminal)
	assert.NotErrorIs(t, err, rpc.ErrStepAbandoned)
}

// fixedUpdateServer answers every Update with one fixed error, to pin the
// client's handling of answers the real server above does not give.
type fixedUpdateServer struct {
	proto.UnimplementedWoodpeckerServer
	err   error
	calls atomic.Int64
}

func (s *fixedUpdateServer) Update(context.Context, *proto.UpdateRequest) (*proto.Empty, error) {
	s.calls.Add(1)
	return new(proto.Empty), s.err
}

// A server from before #397 sends FailedPrecondition with no step state. The
// rejection is still permanent; without the state it must not be read as
// abandonment, which would stop work the server may still want.
func TestGRPCClient_TerminalRejectionWithoutState_IsTerminalOnly(t *testing.T) {
	srv := &fixedUpdateServer{err: status.Error(codes.FailedPrecondition, "step is in terminal state")}
	peer := grpcClientFor(t, srv)

	err := peer.Update(agentCallCtx(t), "117374", rpc.StepState{StepUUID: killedStepUUID})

	require.ErrorIs(t, err, rpc.ErrStepTerminal)
	assert.NotErrorIs(t, err, rpc.ErrStepAbandoned)
	assert.Equal(t, int64(1), srv.calls.Load())
}

// Any other fatal code stays untyped: only the terminal rejection is.
func TestGRPCClient_OtherFatalError_IsNotTerminal(t *testing.T) {
	srv := &fixedUpdateServer{err: status.Error(codes.PermissionDenied, "nope")}
	peer := grpcClientFor(t, srv)

	err := peer.Update(agentCallCtx(t), "117374", rpc.StepState{StepUUID: killedStepUUID})

	require.Error(t, err)
	assert.NotErrorIs(t, err, rpc.ErrStepTerminal)
	assert.Equal(t, codes.PermissionDenied, status.Code(err), "the gRPC status must survive for callers that read it")
}
