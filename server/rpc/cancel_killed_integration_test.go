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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.woodpecker-ci.org/woodpecker/v3/rpc"
	"go.woodpecker-ci.org/woodpecker/v3/server"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/pipeline"
	"go.woodpecker-ci.org/woodpecker/v3/server/pubsub"
)

// #435 part 2, end to end through the real Cancel and the real Done, on a real
// database and queue: a user cancels a bake while compile runs. cleanup opted
// in to `when: status: killed` and must still run; notify ([success, failure])
// must not; the pipeline must end killed, not partial.
func TestCancel_KilledOptInCleanupRunsAndThePipelineEndsKilled(t *testing.T) {
	wireRecordingForge(t)
	s := realStore(t)
	user := &model.User{Login: "owner", ForgeRemoteID: "1"}
	require.NoError(t, s.CreateUser(user))
	repo := &model.Repo{ForgeRemoteID: "1", Owner: "org", Name: "fork", FullName: "org/fork", IsActive: true, UserID: user.ID}
	require.NoError(t, s.CreateRepo(repo))
	agent := &model.Agent{Name: "a", OrgID: model.IDNotSet}
	require.NoError(t, s.AgentCreate(agent))

	now := time.Now().Unix()
	pl := &model.Pipeline{RepoID: repo.ID, Status: model.StatusRunning, Started: now, Event: model.EventManual, Branch: "main"}
	require.NoError(t, s.CreatePipeline(pl))
	compile := &model.Workflow{PipelineID: pl.ID, PID: 1, Name: "compile", State: model.StatusRunning, AgentID: agent.ID, Started: now}
	cleanup := &model.Workflow{PipelineID: pl.ID, PID: 2, Name: "cleanup", State: model.StatusPending, DependsOn: []string{"compile"}}
	notify := &model.Workflow{PipelineID: pl.ID, PID: 3, Name: "notify", State: model.StatusPending, DependsOn: []string{"compile"}}
	require.NoError(t, s.WorkflowsCreate([]*model.Workflow{compile, cleanup, notify}))
	id := func(w *model.Workflow) string { return strconv.FormatInt(w.ID, 10) }

	q := newQueue(t, s)
	prevQ, prevPS := server.Config.Services.Queue, server.Config.Services.Pubsub
	server.Config.Services.Queue, server.Config.Services.Pubsub = q, pubsub.New()
	t.Cleanup(func() { server.Config.Services.Queue, server.Config.Services.Pubsub = prevQ, prevPS })
	require.NoError(t, q.PushAtOnce(context.Background(), []*model.Task{
		{ID: id(compile), PipelineID: pl.ID},
		{ID: id(cleanup), PipelineID: pl.ID, Dependencies: []string{id(compile)}, DepStatus: map[string]model.StatusValue{}, RunOn: []string{"success", "failure", "killed"}},
		{ID: id(notify), PipelineID: pl.ID, Dependencies: []string{id(compile)}, DepStatus: map[string]model.StatusValue{}, RunOn: []string{"success", "failure"}},
	}))
	require.Equal(t, id(compile), claim(t, q, agent.ID).ID)

	require.NoError(t, pipeline.Cancel(context.Background(), nil, s, repo, user, pl, &model.CancelInfo{CanceledByUser: "ops"}, false))

	r := newDoneRPC(q, s)
	ctx := agentCtx(agent.ID)
	// compile's agent stops on the cancel signal and reports it.
	require.NoError(t, r.Done(ctx, id(compile), rpc.WorkflowState{Started: now, Finished: now + 1, Canceled: true}))

	task := claim(t, q, agent.ID)
	require.Equal(t, id(cleanup), task.ID, "cleanup is dispatched after the cancel")
	require.True(t, task.ShouldRun())
	require.NoError(t, r.Init(ctx, id(cleanup), rpc.WorkflowState{Started: now + 2}))
	require.NoError(t, r.Done(ctx, id(cleanup), rpc.WorkflowState{Started: now + 2, Finished: now + 3}))

	got, err := s.GetPipeline(pl.ID)
	require.NoError(t, err)
	assert.Equal(t, model.StatusKilled, got.Status, "a cancelled bake ends killed even though its cleanup succeeded")
	for name, want := range map[*model.Workflow]model.StatusValue{cleanup: model.StatusSuccess, notify: model.StatusSkipped} {
		w, err := s.WorkflowLoad(name.ID)
		require.NoError(t, err)
		assert.Equal(t, want, w.State, name.Name)
	}
}
