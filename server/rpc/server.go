// Copyright 2022 Woodpecker Authors
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

package grpc

import (
	"context"
	"encoding/json"
	"os"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	prometheus_auto "github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog/log"

	"go.woodpecker-ci.org/woodpecker/v3/rpc"
	"go.woodpecker-ci.org/woodpecker/v3/rpc/proto"
	"go.woodpecker-ci.org/woodpecker/v3/server/logging"
	"go.woodpecker-ci.org/woodpecker/v3/server/pubsub"
	"go.woodpecker-ci.org/woodpecker/v3/server/queue"
	"go.woodpecker-ci.org/woodpecker/v3/server/store"
	"go.woodpecker-ci.org/woodpecker/v3/version"
)

// WoodpeckerServer is a grpc server implementation.
type WoodpeckerServer struct {
	proto.UnimplementedWoodpeckerServer
	peer RPC
}

// NewRPC creates the business logic peer for both gRPC and WebSocket transports.
func NewRPC(queue queue.Queue, logger logging.Log, pubsub *pubsub.Publisher, store store.Store) *RPC {
	pipelineTime := prometheus_auto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "woodpecker",
		Name:      "pipeline_time",
		Help:      "Pipeline time.",
	}, []string{"repo", "branch", "status", "pipeline"})
	pipelineCount := prometheus_auto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "woodpecker",
		Name:      "pipeline_count",
		Help:      "Pipeline count.",
	}, []string{"repo", "branch", "status", "pipeline"})
	return &RPC{
		store:          store,
		queue:          queue,
		pubsub:         pubsub,
		logger:         logger,
		pipelineTime:   pipelineTime,
		pipelineCount:  pipelineCount,
		deployPatterns: loadDeployPatterns(),
	}
}

// NewWoodpeckerServer wraps an existing RPC peer as a gRPC server.
// Use NewRPC() first to create the shared peer, then pass it here.
func NewWoodpeckerServer(peer *RPC) proto.WoodpeckerServer {
	return &WoodpeckerServer{peer: *peer}
}

// loadDeployPatterns reads WOODPECKER_DEPLOY_PATTERNS env var.
// Default: deploy,version-bump,sync-back. Set to empty to disable.
func loadDeployPatterns() []string {
	raw := os.Getenv("WOODPECKER_DEPLOY_PATTERNS")
	if raw == "" {
		return []string{"deploy", "version-bump", "sync-back"}
	}
	if raw == "none" {
		return nil
	}
	var patterns []string
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			patterns = append(patterns, p)
		}
	}
	if len(patterns) > 0 {
		log.Info().Msgf("Deploy auto-routing patterns: %v", patterns)
	}
	return patterns
}

// Version returns the server- & grpc-version.
func (s *WoodpeckerServer) Version(_ context.Context, _ *proto.Empty) (*proto.VersionResponse, error) {
	return &proto.VersionResponse{
		GrpcVersion:   proto.Version,
		ServerVersion: version.String(),
	}, nil
}

// Next blocks until it provides the next workflow to execute from the queue.
func (s *WoodpeckerServer) Next(c context.Context, req *proto.NextRequest) (*proto.NextResponse, error) {
	filter := rpc.Filter{
		Labels: req.GetFilter().GetLabels(),
	}

	res := new(proto.NextResponse)
	pipeline, err := s.peer.Next(c, filter)
	if err != nil || pipeline == nil {
		return res, err
	}

	res.Workflow = new(proto.Workflow)
	res.Workflow.Id = pipeline.ID
	res.Workflow.Timeout = pipeline.Timeout
	res.Workflow.Payload, err = json.Marshal(pipeline.Config)

	return res, err
}

// Init let agent signals to server the workflow is initialized.
func (s *WoodpeckerServer) Init(c context.Context, req *proto.InitRequest) (*proto.Empty, error) {
	state := rpc.WorkflowState{
		Started:  req.GetState().GetStarted(),
		Finished: req.GetState().GetFinished(),
		Error:    req.GetState().GetError(),
	}
	res := new(proto.Empty)
	err := s.peer.Init(c, req.GetId(), state)
	return res, err
}

// Update let agent updates the step state at the server.
func (s *WoodpeckerServer) Update(c context.Context, req *proto.UpdateRequest) (*proto.Empty, error) {
	state := rpc.StepState{
		StepUUID: req.GetState().GetStepUuid(),
		Started:  req.GetState().GetStarted(),
		Finished: req.GetState().GetFinished(),
		Exited:   req.GetState().GetExited(),
		Error:    req.GetState().GetError(),
		ExitCode: int(req.GetState().GetExitCode()),
		Canceled: req.GetState().GetCanceled(),
	}
	res := new(proto.Empty)
	err := s.peer.Update(c, req.GetId(), state)
	return res, err
}

// Done let agent signal to server the workflow has stopped.
func (s *WoodpeckerServer) Done(c context.Context, req *proto.DoneRequest) (*proto.Empty, error) {
	state := rpc.WorkflowState{
		Started:  req.GetState().GetStarted(),
		Finished: req.GetState().GetFinished(),
		Error:    req.GetState().GetError(),
		Canceled: req.GetState().GetCanceled(),
	}
	res := new(proto.Empty)
	err := s.peer.Done(c, req.GetId(), state)
	return res, err
}

// Wait blocks until the workflow is complete.
// Also signals via err if workflow got canceled.
func (s *WoodpeckerServer) Wait(c context.Context, req *proto.WaitRequest) (*proto.WaitResponse, error) {
	res := new(proto.WaitResponse)
	canceled, err := s.peer.Wait(c, req.GetId())
	res.Canceled = canceled
	return res, err
}

// Extend extends the workflow deadline.
func (s *WoodpeckerServer) Extend(c context.Context, req *proto.ExtendRequest) (*proto.Empty, error) {
	res := new(proto.Empty)
	err := s.peer.Extend(c, req.GetId())
	return res, err
}

func (s *WoodpeckerServer) Log(c context.Context, req *proto.LogRequest) (*proto.Empty, error) {
	var (
		entries  []*rpc.LogEntry
		stepUUID string
	)

	write := func() error {
		if len(entries) > 0 {
			if err := s.peer.Log(c, stepUUID, entries); err != nil {
				log.Error().Err(err).Msg("could not write log entries")
				return err
			}
		}
		return nil
	}

	for _, reqEntry := range req.GetLogEntries() {
		entry := &rpc.LogEntry{
			Data:     reqEntry.GetData(),
			Line:     int(reqEntry.GetLine()),
			Time:     reqEntry.GetTime(),
			StepUUID: reqEntry.GetStepUuid(),
			Type:     int(reqEntry.GetType()),
		}
		if entry.StepUUID != stepUUID {
			_ = write()
			stepUUID = entry.StepUUID
			entries = entries[:0]
		}
		entries = append(entries, entry)
	}

	res := new(proto.Empty)
	err := write()
	return res, err
}

// RegisterAgent register our agent to the server.
func (s *WoodpeckerServer) RegisterAgent(c context.Context, req *proto.RegisterAgentRequest) (*proto.RegisterAgentResponse, error) {
	res := new(proto.RegisterAgentResponse)
	agentInfo := req.GetInfo()
	agentID, err := s.peer.RegisterAgent(c, rpc.AgentInfo{
		Version:      agentInfo.GetVersion(),
		Platform:     agentInfo.GetPlatform(),
		Backend:      agentInfo.GetBackend(),
		Capacity:     int(agentInfo.GetCapacity()),
		CustomLabels: agentInfo.GetCustomLabels(),
	})
	res.AgentId = agentID
	return res, err
}

// UnregisterAgent unregister our agent from the server.
func (s *WoodpeckerServer) UnregisterAgent(ctx context.Context, _ *proto.Empty) (*proto.Empty, error) {
	err := s.peer.UnregisterAgent(ctx)
	return new(proto.Empty), err
}

// ReportHealth reports health status of the agent to the server.
func (s *WoodpeckerServer) ReportHealth(c context.Context, req *proto.ReportHealthRequest) (*proto.Empty, error) {
	res := new(proto.Empty)
	err := s.peer.ReportHealth(c, req.GetStatus())
	return res, err
}
