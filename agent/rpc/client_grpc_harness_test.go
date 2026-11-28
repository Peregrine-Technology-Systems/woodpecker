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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.woodpecker-ci.org/woodpecker/v3/rpc"
	"go.woodpecker-ci.org/woodpecker/v3/rpc/proto"
)

// The gRPC agent client had no tests before #397. These run the real client
// against a scripted server over an in-memory connection and pin each call's
// answer to every class of gRPC error: retried, fatal, and canceled.

// scriptedServer answers each RPC with the next scripted error for that
// method, then with success once the script runs out.
type scriptedServer struct {
	proto.UnimplementedWoodpeckerServer
	mu       sync.Mutex
	script   map[string][]error
	calls    map[string]int
	logs     [][]*proto.LogEntry
	workflow *proto.Workflow
}

func newScriptedServer() *scriptedServer {
	return &scriptedServer{script: map[string][]error{}, calls: map[string]int{}}
}

func (s *scriptedServer) next(method string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[method]++
	if q := s.script[method]; len(q) > 0 {
		s.script[method] = q[1:]
		return q[0]
	}
	return nil
}

func (s *scriptedServer) count(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[method]
}

func (s *scriptedServer) Version(context.Context, *proto.Empty) (*proto.VersionResponse, error) {
	if err := s.next("Version"); err != nil {
		return nil, err
	}
	return &proto.VersionResponse{GrpcVersion: 3, ServerVersion: "v3.13.0-pts.634"}, nil
}

func (s *scriptedServer) Next(context.Context, *proto.NextRequest) (*proto.NextResponse, error) {
	if err := s.next("Next"); err != nil {
		return nil, err
	}
	return &proto.NextResponse{Workflow: s.workflow}, nil
}

func (s *scriptedServer) Wait(context.Context, *proto.WaitRequest) (*proto.WaitResponse, error) {
	if err := s.next("Wait"); err != nil {
		return nil, err
	}
	return &proto.WaitResponse{Canceled: true}, nil
}

func (s *scriptedServer) Init(context.Context, *proto.InitRequest) (*proto.Empty, error) {
	return &proto.Empty{}, s.next("Init")
}

func (s *scriptedServer) Done(context.Context, *proto.DoneRequest) (*proto.Empty, error) {
	return &proto.Empty{}, s.next("Done")
}

func (s *scriptedServer) Extend(context.Context, *proto.ExtendRequest) (*proto.Empty, error) {
	return &proto.Empty{}, s.next("Extend")
}

func (s *scriptedServer) Update(context.Context, *proto.UpdateRequest) (*proto.Empty, error) {
	return &proto.Empty{}, s.next("Update")
}

func (s *scriptedServer) Log(_ context.Context, req *proto.LogRequest) (*proto.Empty, error) {
	err := s.next("Log")
	if err == nil {
		s.mu.Lock()
		s.logs = append(s.logs, req.GetLogEntries())
		s.mu.Unlock()
	}
	return &proto.Empty{}, err
}

func (s *scriptedServer) sentLogs() [][]*proto.LogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]*proto.LogEntry(nil), s.logs...)
}

func (s *scriptedServer) RegisterAgent(context.Context, *proto.RegisterAgentRequest) (*proto.RegisterAgentResponse, error) {
	if err := s.next("RegisterAgent"); err != nil {
		return nil, err
	}
	return &proto.RegisterAgentResponse{AgentId: 61493}, nil
}

func (s *scriptedServer) UnregisterAgent(context.Context, *proto.Empty) (*proto.Empty, error) {
	return &proto.Empty{}, s.next("UnregisterAgent")
}

func (s *scriptedServer) ReportHealth(context.Context, *proto.ReportHealthRequest) (*proto.Empty, error) {
	return &proto.Empty{}, s.next("ReportHealth")
}

// cancelAfterCallKey marks a call context whose cancel func the interceptor
// runs once the RPC has returned, so a test reaches the "context ended during
// the retry backoff" branch deterministically.
type cancelAfterCallKey struct{}

func cancelAfterCall(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	err := invoker(ctx, method, req, reply, cc, opts...)
	if cancel, ok := ctx.Value(cancelAfterCallKey{}).(context.CancelFunc); ok {
		cancel()
	}
	return err
}

func endsAfterFirstCall(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return context.WithValue(ctx, cancelAfterCallKey{}, cancel)
}

func alreadyCanceled() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

var (
	errRetryable = status.Error(codes.Unavailable, "server restarting")
	errFatal     = status.Error(codes.PermissionDenied, "agent not allowed")
	errCanceled  = status.Error(codes.Canceled, "stream reset by server")
)

// errorCall is one client method that returns only an error, under the name
// the scripted server knows it by.
type errorCall struct {
	method string
	call   func(context.Context, rpc.Peer) error
}

func errorCalls() []errorCall {
	return []errorCall{
		{"Init", func(ctx context.Context, p rpc.Peer) error { return p.Init(ctx, "1", rpc.WorkflowState{Started: 1}) }},
		{"Done", func(ctx context.Context, p rpc.Peer) error { return p.Done(ctx, "1", rpc.WorkflowState{Finished: 2}) }},
		{"Extend", func(ctx context.Context, p rpc.Peer) error { return p.Extend(ctx, "1") }},
		{"Update", func(ctx context.Context, p rpc.Peer) error { return p.Update(ctx, "1", rpc.StepState{StepUUID: "s"}) }},
		{"ReportHealth", func(ctx context.Context, p rpc.Peer) error { return p.ReportHealth(ctx) }},
		{"Log", func(ctx context.Context, p rpc.Peer) error {
			return p.(*client).sendLogs(ctx, []*proto.LogEntry{{StepUuid: "s", Line: 1}})
		}},
	}
}

func TestGRPCClient_ErrorCalls(t *testing.T) {
	for _, ec := range errorCalls() {
		t.Run(ec.method+"/retries a transient error, then succeeds", func(t *testing.T) {
			srv := newScriptedServer()
			srv.script[ec.method] = []error{errRetryable, status.Error(codes.Internal, "blip")}
			err := ec.call(agentCallCtx(t), grpcClientFor(t, srv))
			require.NoError(t, err)
			assert.Equal(t, 3, srv.count(ec.method))
		})
		t.Run(ec.method+"/returns a fatal error after one attempt", func(t *testing.T) {
			srv := newScriptedServer()
			srv.script[ec.method] = []error{errFatal}
			err := ec.call(agentCallCtx(t), grpcClientFor(t, srv))
			assert.Equal(t, codes.PermissionDenied, status.Code(err))
			assert.Equal(t, 1, srv.count(ec.method))
		})
		t.Run(ec.method+"/returns a server-side cancel", func(t *testing.T) {
			srv := newScriptedServer()
			srv.script[ec.method] = []error{errCanceled}
			err := ec.call(agentCallCtx(t), grpcClientFor(t, srv))
			assert.Equal(t, codes.Canceled, status.Code(err))
		})
		t.Run(ec.method+"/is quiet when the caller canceled", func(t *testing.T) {
			err := ec.call(alreadyCanceled(), grpcClientFor(t, newScriptedServer()))
			assert.NoError(t, err)
		})
		t.Run(ec.method+"/stops retrying when the caller's context ends", func(t *testing.T) {
			srv := newScriptedServer()
			srv.script[ec.method] = []error{errRetryable}
			err := ec.call(endsAfterFirstCall(t), grpcClientFor(t, srv))
			require.ErrorIs(t, err, context.Canceled)
			assert.Equal(t, 1, srv.count(ec.method))
		})
	}
}

func TestGRPCClient_Version(t *testing.T) {
	srv := newScriptedServer()
	peer := grpcClientFor(t, srv)

	v, err := peer.Version(agentCallCtx(t))
	require.NoError(t, err)
	assert.Equal(t, &rpc.Version{GrpcVersion: 3, ServerVersion: "v3.13.0-pts.634"}, v)

	srv.script["Version"] = []error{errFatal}
	v, err = peer.Version(agentCallCtx(t))
	assert.Nil(t, v)
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
}

func TestGRPCClient_Next(t *testing.T) {
	t.Run("decodes the workflow after a transient error", func(t *testing.T) {
		srv := newScriptedServer()
		srv.workflow = &proto.Workflow{Id: "117400", Timeout: 60, Payload: []byte(`{"stages":[]}`)}
		srv.script["Next"] = []error{
			status.Error(codes.Unavailable, `transport: received GOAWAY with "too_many_pings"`),
			errRetryable,
		}
		wf, err := grpcClientFor(t, srv).Next(agentCallCtx(t), rpc.Filter{Labels: map[string]string{"tier": "spot"}})
		require.NoError(t, err)
		require.NotNil(t, wf)
		assert.Equal(t, "117400", wf.ID)
		assert.Equal(t, int64(60), wf.Timeout)
		assert.NotNil(t, wf.Config)
		assert.Equal(t, 3, srv.count("Next"))
	})
	t.Run("keeps a workflow whose config does not decode", func(t *testing.T) {
		srv := newScriptedServer()
		srv.workflow = &proto.Workflow{Id: "117401", Payload: []byte(`not json`)}
		wf, err := grpcClientFor(t, srv).Next(agentCallCtx(t), rpc.Filter{})
		require.NoError(t, err)
		assert.Equal(t, "117401", wf.ID)
	})
	t.Run("no work is nil, nil", func(t *testing.T) {
		wf, err := grpcClientFor(t, newScriptedServer()).Next(agentCallCtx(t), rpc.Filter{})
		assert.NoError(t, err)
		assert.Nil(t, wf)
	})
	t.Run("returns a fatal error", func(t *testing.T) {
		srv := newScriptedServer()
		srv.script["Next"] = []error{errFatal}
		wf, err := grpcClientFor(t, srv).Next(agentCallCtx(t), rpc.Filter{})
		assert.Nil(t, wf)
		assert.Equal(t, codes.PermissionDenied, status.Code(err))
	})
	t.Run("returns a server-side cancel", func(t *testing.T) {
		srv := newScriptedServer()
		srv.script["Next"] = []error{errCanceled}
		_, err := grpcClientFor(t, srv).Next(agentCallCtx(t), rpc.Filter{})
		assert.Equal(t, codes.Canceled, status.Code(err))
	})
	t.Run("is quiet when the caller canceled", func(t *testing.T) {
		wf, err := grpcClientFor(t, newScriptedServer()).Next(alreadyCanceled(), rpc.Filter{})
		assert.NoError(t, err)
		assert.Nil(t, wf)
	})
	t.Run("stops retrying when the caller's context ends", func(t *testing.T) {
		srv := newScriptedServer()
		srv.script["Next"] = []error{errRetryable}
		wf, err := grpcClientFor(t, srv).Next(endsAfterFirstCall(t), rpc.Filter{})
		assert.NoError(t, err)
		assert.Nil(t, wf)
	})
}

// Wait must never report an error for a dropped or failing connection: the
// runner cancels the workflow on one (#3496).
func TestGRPCClient_Wait(t *testing.T) {
	t.Run("retries a dropped connection and a transient error", func(t *testing.T) {
		srv := newScriptedServer()
		srv.script["Wait"] = []error{errCanceled, errRetryable}
		canceled, err := grpcClientFor(t, srv).Wait(agentCallCtx(t), "1")
		require.NoError(t, err)
		assert.True(t, canceled)
		assert.Equal(t, 3, srv.count("Wait"))
	})
	t.Run("a fatal error does not cancel the workflow", func(t *testing.T) {
		srv := newScriptedServer()
		srv.script["Wait"] = []error{errFatal}
		canceled, err := grpcClientFor(t, srv).Wait(agentCallCtx(t), "1")
		assert.NoError(t, err)
		assert.False(t, canceled)
	})
	t.Run("is quiet when the caller canceled", func(t *testing.T) {
		canceled, err := grpcClientFor(t, newScriptedServer()).Wait(alreadyCanceled(), "1")
		assert.NoError(t, err)
		assert.False(t, canceled)
	})
	t.Run("stops retrying when the caller's context ends", func(t *testing.T) {
		srv := newScriptedServer()
		srv.script["Wait"] = []error{errRetryable}
		_, err := grpcClientFor(t, srv).Wait(endsAfterFirstCall(t), "1")
		assert.ErrorIs(t, err, context.Canceled)
	})
}

func TestGRPCClient_RegisterAndUnregister(t *testing.T) {
	srv := newScriptedServer()
	peer := grpcClientFor(t, srv)

	id, err := peer.RegisterAgent(agentCallCtx(t), rpc.AgentInfo{Platform: "linux/amd64", Capacity: 1})
	require.NoError(t, err)
	assert.Equal(t, int64(61493), id)
	require.NoError(t, peer.UnregisterAgent(agentCallCtx(t)))

	srv.script["RegisterAgent"] = []error{errFatal}
	srv.script["UnregisterAgent"] = []error{errFatal}
	_, err = peer.RegisterAgent(agentCallCtx(t), rpc.AgentInfo{})
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	assert.Equal(t, codes.PermissionDenied, status.Code(peer.UnregisterAgent(agentCallCtx(t))))
}

// The log drain batches queued entries and sends them when the batch reaches
// its size limit, when the flush period passes, and when the client closes.
func TestGRPCClient_LogDrain(t *testing.T) {
	t.Run("flushes after the flush period", func(t *testing.T) {
		srv := newScriptedServer()
		peer := grpcClientFor(t, srv)
		peer.EnqueueLog(&rpc.LogEntry{StepUUID: "s", Line: 1, Data: []byte("a")})
		peer.EnqueueLog(&rpc.LogEntry{StepUUID: "s", Line: 2, Data: []byte("b")})
		require.Eventually(t, func() bool { return len(srv.sentLogs()) == 1 }, 3*maxLogFlushPeriod, 10*time.Millisecond)
		assert.Len(t, srv.sentLogs()[0], 2, "queued entries go in one batch")
	})
	t.Run("sends a batch that reaches the size limit at once", func(t *testing.T) {
		srv := newScriptedServer()
		peer := grpcClientFor(t, srv)
		peer.EnqueueLog(&rpc.LogEntry{StepUUID: "s", Line: 1, Data: []byte(strings.Repeat("x", maxLogBatchSize))})
		require.Eventually(t, func() bool { return len(srv.sentLogs()) == 1 }, maxLogFlushPeriod/2, 5*time.Millisecond)
	})
	// The drain is driven by the log channel closing. Close also closes the
	// connection at once, so this test closes only the channel: it pins the
	// drain, not Close's ordering.
	t.Run("sends what is queued when the log channel closes, and drops a batch the server refuses", func(t *testing.T) {
		srv := newScriptedServer()
		srv.script["Log"] = []error{errFatal}
		conn := bufconnClientConn(t, srv)
		t.Cleanup(func() { _ = conn.Close() })
		c := NewGrpcClient(context.Background(), conn).(*client)
		c.EnqueueLog(&rpc.LogEntry{StepUUID: "s", Line: 1})
		require.Eventually(t, func() bool { return srv.count("Log") == 1 }, 3*maxLogFlushPeriod, 10*time.Millisecond)
		c.EnqueueLog(&rpc.LogEntry{StepUUID: "s", Line: 2})
		close(c.logs)
		require.Eventually(t, func() bool { return len(srv.sentLogs()) == 1 }, time.Second, 5*time.Millisecond)
		assert.Equal(t, int32(2), srv.sentLogs()[0][0].GetLine(), "the refused batch is not resent")
	})
}

func TestGRPCClient_CloseClosesTheConnection(t *testing.T) {
	conn := bufconnClientConn(t, newScriptedServer())
	c := NewGrpcClient(context.Background(), conn).(*client)
	require.NoError(t, c.Close())
	assert.Error(t, conn.Close(), "Close must have closed the connection already")
}
