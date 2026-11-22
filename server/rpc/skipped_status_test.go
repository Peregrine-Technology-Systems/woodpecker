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

package grpc

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	grpcMetadata "google.golang.org/grpc/metadata"

	"go.woodpecker-ci.org/woodpecker/v3/rpc"
	"go.woodpecker-ci.org/woodpecker/v3/server"
	forge_mocks "go.woodpecker-ci.org/woodpecker/v3/server/forge/mocks"
	"go.woodpecker-ci.org/woodpecker/v3/server/logging"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/pubsub"
	"go.woodpecker-ci.org/woodpecker/v3/server/queue"
	manager_mocks "go.woodpecker-ci.org/woodpecker/v3/server/services/mocks"
	"go.woodpecker-ci.org/woodpecker/v3/server/store"
)

// #403, through the real entry point: an agent reports ci failed, the server
// skips promote (depends_on: [ci]) itself (#382), and promote's commit status
// must leave "pending". Real database, real queue, real Done; only the forge is
// a recorder.

// statusRecorder records every forge Status call as workflow name → state.
type statusRecorder struct {
	mu    sync.Mutex
	posts map[string][]model.StatusValue
}

func (r *statusRecorder) record(args mock.Arguments) {
	wf := args.Get(4).(*model.Workflow)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.posts[wf.Name] = append(r.posts[wf.Name], wf.State)
}

func (r *statusRecorder) get(name string) []model.StatusValue {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]model.StatusValue(nil), r.posts[name]...)
}

func wireRecordingForge(t *testing.T) *statusRecorder {
	t.Helper()
	rec := &statusRecorder{posts: map[string][]model.StatusValue{}}
	f := forge_mocks.NewMockForge(t)
	f.On("Status", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Run(rec.record).Return(nil)
	m := manager_mocks.NewMockManager(t)
	m.On("ForgeFromRepo", mock.Anything).Return(f, nil)
	prev := server.Config.Services.Manager
	server.Config.Services.Manager = m
	t.Cleanup(func() { server.Config.Services.Manager = prev })
	return rec
}

// newDoneRPC is a full RPC over its own, unregistered metrics.
func newDoneRPC(q queue.Queue, s store.Store) *RPC {
	gauge := func() *prometheus.GaugeVec {
		return prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "g"}, []string{"repo", "branch", "status", "pipeline"})
	}
	counter := func() *prometheus.CounterVec {
		return prometheus.NewCounterVec(prometheus.CounterOpts{Name: "c"}, []string{"repo", "branch", "status", "pipeline"})
	}
	return &RPC{
		queue: q, store: s, pubsub: pubsub.New(), logger: logging.New(),
		pipelineTime: gauge(), pipelineCount: counter(), ciPipelineTime: gauge(), ciPipelineCount: counter(),
		preemptRequeues: map[int64]int{},
	}
}

func agentCtx(agentID int64) context.Context {
	return grpcMetadata.NewIncomingContext(context.Background(),
		grpcMetadata.Pairs("agent_id", strconv.FormatInt(agentID, 10)))
}

func TestDone_SkippedDependentGetsATerminalCommitStatus(t *testing.T) {
	rec := wireRecordingForge(t)
	s := realStore(t)
	user := &model.User{Login: "owner", ForgeRemoteID: "1"}
	require.NoError(t, s.CreateUser(user))
	repo := &model.Repo{ForgeRemoteID: "1", Owner: "org", Name: "app", FullName: "org/app", IsActive: true, UserID: user.ID}
	require.NoError(t, s.CreateRepo(repo))
	agent := &model.Agent{Name: "a", OrgID: model.IDNotSet}
	require.NoError(t, s.AgentCreate(agent))

	now := time.Now().Unix()
	pl := &model.Pipeline{RepoID: repo.ID, Status: model.StatusRunning, Started: now, Event: model.EventPush, Branch: "development"}
	require.NoError(t, s.CreatePipeline(pl))
	ci := &model.Workflow{PipelineID: pl.ID, PID: 1, Name: "ci", State: model.StatusRunning, AgentID: agent.ID, Started: now}
	promote := &model.Workflow{PipelineID: pl.ID, PID: 2, Name: "promote", State: model.StatusPending, DependsOn: []string{"ci"}}
	require.NoError(t, s.WorkflowsCreate([]*model.Workflow{ci, promote}))
	ciID, promoteID := strconv.FormatInt(ci.ID, 10), strconv.FormatInt(promote.ID, 10)

	q := newQueue(t, s)
	require.NoError(t, q.PushAtOnce(context.Background(), []*model.Task{
		{ID: ciID, PipelineID: pl.ID},
		// tier label no agent in this test advertises: only the server can skip it
		{ID: promoteID, PipelineID: pl.ID, Labels: map[string]string{"tier": "spot"}, Dependencies: []string{ciID}, DepStatus: map[string]model.StatusValue{}},
	}))
	claim(t, q, agent.ID)

	require.NoError(t, newDoneRPC(q, s).Done(agentCtx(agent.ID), ciID,
		rpc.WorkflowState{Started: now, Finished: now + 1, Error: "tests failed"}))

	got, err := s.WorkflowLoad(promote.ID)
	require.NoError(t, err)
	require.Equal(t, model.StatusSkipped, got.State, "precondition: the server skipped promote (#382)")
	assert.Equal(t, []model.StatusValue{model.StatusSkipped}, rec.get("promote"),
		"a workflow the server skips must post its terminal status, or its pending check never clears")
	assert.Equal(t, []model.StatusValue{model.StatusFailure}, rec.get("ci"))
}
