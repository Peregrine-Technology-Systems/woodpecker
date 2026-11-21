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
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/pipeline"
	"go.woodpecker-ci.org/woodpecker/v3/server/queue"
	"go.woodpecker-ci.org/woodpecker/v3/server/store"
	"go.woodpecker-ci.org/woodpecker/v3/server/store/datastore"
	"go.woodpecker-ci.org/woodpecker/v3/shared/constant"
)

// #400, end to end over a real database, the real fifo, the real persistent
// wrapper and the real release: a server restarts while agent 7 runs a
// workflow that has done real work, and agent 7 never renews it.

func realStore(t *testing.T) store.Store {
	t.Helper()
	s, err := datastore.NewEngine(&store.Opts{Driver: "sqlite3", Config: filepath.Join(t.TempDir(), "wp.sqlite")})
	require.NoError(t, err)
	require.NoError(t, s.Migrate(context.Background(), true))
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// runningPipeline stores a running pipeline whose one workflow runs on agentID
// and whose step has written a log line, i.e. it did real work (#235).
func runningPipeline(t *testing.T, s store.Store, repo *model.Repo, agentID int64) (*model.Pipeline, *model.Workflow) {
	t.Helper()
	now := time.Now().Unix()
	pl := &model.Pipeline{RepoID: repo.ID, Status: model.StatusRunning, Started: now}
	require.NoError(t, s.CreatePipeline(pl))
	step := &model.Step{PipelineID: pl.ID, PPID: 1, PID: 2, UUID: "step-" + strconv.FormatInt(pl.ID, 10), Name: "deploy", State: model.StatusRunning, Started: now}
	wf := &model.Workflow{PipelineID: pl.ID, PID: 1, Name: "deploy", State: model.StatusRunning, AgentID: agentID, Started: now, Children: []*model.Step{step}}
	require.NoError(t, s.WorkflowsCreate([]*model.Workflow{wf}))
	require.NoError(t, s.LogAppend(step, []*model.LogEntry{{StepID: step.ID, Line: 1, Data: []byte("deploying")}}))
	return pl, wf
}

func newQueue(t *testing.T, s store.Store) queue.Queue {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return queue.WithTaskStore(ctx, queue.NewMemoryQueue(ctx), s)
}

func claim(t *testing.T, q queue.Queue, agentID int64) *model.Task {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	task, err := q.Poll(ctx, agentID, func(*model.Task) (bool, int) { return true, 1 })
	require.NoError(t, err)
	require.NotNil(t, task)
	return task
}

func TestRestart_UnrenewedRestoredWorkIsKilledAloneAndNotAtBoot(t *testing.T) {
	origTimeout := constant.TaskTimeout
	constant.TaskTimeout = 400 * time.Millisecond
	t.Cleanup(func() { constant.TaskTimeout = origTimeout })

	s := realStore(t)
	repo := &model.Repo{ForgeRemoteID: "1", Owner: "org", Name: "infra", FullName: "org/infra", IsActive: true}
	require.NoError(t, s.CreateRepo(repo))
	pl, wf := runningPipeline(t, s, repo, 7)

	before := newQueue(t, s)
	require.NoError(t, before.PushAtOnce(context.Background(), []*model.Task{{ID: strconv.FormatInt(wf.ID, 10), PipelineID: pl.ID}}))
	claim(t, before, 7)

	// --- restart ---
	after := newQueue(t, s)
	rpc := NewRPCForTesting(after, s)
	after.SetRestoredExpiryFn(func(tasks []*model.Task) { rpc.ReleaseTasks(context.Background(), tasks) })

	assert.Equal(t, 0, pipeline.ReconcileOrphanedAtStartup(s, after),
		"the startup reconcile must not kill a pipeline whose work an agent holds")

	// Agent 7 restarted with the server under the same id and took new work.
	otherPl, otherWf := runningPipeline(t, s, repo, 7)
	otherID := strconv.FormatInt(otherWf.ID, 10)
	require.NoError(t, after.PushAtOnce(context.Background(), []*model.Task{{ID: otherID, PipelineID: otherPl.ID}}))
	claim(t, after, 7)

	// Agent 7 renews only its new work; the restored work is never renewed.
	require.Eventually(t, func() bool {
		_ = after.Extend(context.Background(), 7, otherID)
		got, err := s.WorkflowLoad(wf.ID)
		return err == nil && got.State == model.StatusKilled
	}, 5*time.Second, 50*time.Millisecond, "unrenewed restored work that ran must be killed, in bounded time")

	killed, err := s.GetPipeline(pl.ID)
	require.NoError(t, err)
	assert.Equal(t, model.StatusKilled, killed.Status)
	// Intended: an agent that held work across a restart and never came back for
	// it is an agent disconnect, with the same forge-status and restart handling.
	assert.Equal(t, "agent_disconnect", killed.KillReason)

	rows, err := s.TaskList()
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, r := range rows {
		ids[r.ID] = true
	}
	assert.False(t, ids[strconv.FormatInt(wf.ID, 10)], "the killed work's row is removed")
	assert.True(t, ids[otherID], "the new work's row is untouched")

	untouched, err := s.WorkflowLoad(otherWf.ID)
	require.NoError(t, err)
	assert.Equal(t, model.StatusRunning, untouched.State, "work the same agent id took after the restart is not released")
}
