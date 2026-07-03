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
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"

	"go.woodpecker-ci.org/woodpecker/v3/rpc"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/queue"
	queue_mocks "go.woodpecker-ci.org/woodpecker/v3/server/queue/mocks"
	store_mocks "go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
)

func TestRegisterAgent(t *testing.T) {
	t.Run("When existing agent Name is empty it should update Name with hostname from metadata", func(t *testing.T) {
		store := store_mocks.NewMockStore(t)
		storeAgent := new(model.Agent)
		storeAgent.ID = 1337
		updatedAgent := model.Agent{
			ID:          1337,
			Created:     0,
			Updated:     0,
			Name:        "hostname",
			OwnerID:     0,
			Token:       "",
			LastContact: 0,
			Platform:    "platform",
			Backend:     "backend",
			Capacity:    2,
			Version:     "version",
			NoSchedule:  false,
		}

		store.On("AgentFind", int64(1337)).Once().Return(storeAgent, nil)
		store.On("AgentUpdate", &updatedAgent).Once().Return(nil)
		grpc := RPC{
			store: store,
		}
		ctx := metadata.NewIncomingContext(
			t.Context(),
			metadata.Pairs("hostname", "hostname", "agent_id", "1337"),
		)
		agentID, err := grpc.RegisterAgent(ctx, rpc.AgentInfo{
			Version:  "version",
			Platform: "platform",
			Backend:  "backend",
			Capacity: 2,
		})
		require.NoError(t, err)

		assert.EqualValues(t, 1337, agentID)
	})

	t.Run("When existing agent hostname is present it should not update the hostname", func(t *testing.T) {
		store := store_mocks.NewMockStore(t)
		storeAgent := new(model.Agent)
		storeAgent.ID = 1337
		storeAgent.Name = "originalHostname"
		updatedAgent := model.Agent{
			ID:          1337,
			Created:     0,
			Updated:     0,
			Name:        "originalHostname",
			OwnerID:     0,
			Token:       "",
			LastContact: 0,
			Platform:    "platform",
			Backend:     "backend",
			Capacity:    2,
			Version:     "version",
			NoSchedule:  false,
		}

		store.On("AgentFind", int64(1337)).Once().Return(storeAgent, nil)
		store.On("AgentUpdate", &updatedAgent).Once().Return(nil)
		grpc := RPC{
			store: store,
		}
		ctx := metadata.NewIncomingContext(
			t.Context(),
			metadata.Pairs("hostname", "newHostname", "agent_id", "1337"),
		)
		agentID, err := grpc.RegisterAgent(ctx, rpc.AgentInfo{
			Version:  "version",
			Platform: "platform",
			Backend:  "backend",
			Capacity: 2,
		})
		require.NoError(t, err)

		assert.EqualValues(t, 1337, agentID)
	})
}

func TestUpdateAgentLastWork(t *testing.T) {
	t.Run("When last work was never updated it should update last work timestamp", func(t *testing.T) {
		agent := model.Agent{
			LastWork: 0,
		}
		store := store_mocks.NewMockStore(t)
		rpc := RPC{
			store: store,
		}
		store.On("AgentUpdate", mock.Anything).Once().Return(nil)

		err := rpc.updateAgentLastWork(&agent)
		assert.NoError(t, err)

		assert.NotZero(t, agent.LastWork)
	})

	t.Run("When last work was updated over a minute ago it should update last work timestamp", func(t *testing.T) {
		lastWork := time.Now().Add(-time.Hour).Unix()
		agent := model.Agent{
			LastWork: lastWork,
		}
		store := store_mocks.NewMockStore(t)
		rpc := RPC{
			store: store,
		}
		store.On("AgentUpdate", mock.Anything).Once().Return(nil)

		err := rpc.updateAgentLastWork(&agent)
		assert.NoError(t, err)

		assert.NotEqual(t, lastWork, agent.LastWork)
	})

	t.Run("When last work was updated in the last minute it should not update last work timestamp again", func(t *testing.T) {
		lastWork := time.Now().Add(-time.Second * 30).Unix()
		agent := model.Agent{
			LastWork: lastWork,
		}
		rpc := RPC{}

		err := rpc.updateAgentLastWork(&agent)
		assert.NoError(t, err)

		assert.Equal(t, lastWork, agent.LastWork)
	})
}

// nextTestCtx builds an incoming gRPC context carrying agent_id, matching the
// shape RPC.Next() reads via getAgentFromContext.
func nextTestCtx(t *testing.T, agentID int64) context.Context {
	t.Helper()
	return metadata.NewIncomingContext(
		t.Context(),
		metadata.Pairs("agent_id", strconv.FormatInt(agentID, 10)),
	)
}

func TestLoadNoScheduleOverrideLabel(t *testing.T) {
	t.Run("defaults to backend when unset", func(t *testing.T) {
		prev, wasSet := os.LookupEnv("WOODPECKER_NO_SCHEDULE_OVERRIDE_LABEL")
		require.NoError(t, os.Unsetenv("WOODPECKER_NO_SCHEDULE_OVERRIDE_LABEL"))
		t.Cleanup(func() {
			if wasSet {
				_ = os.Setenv("WOODPECKER_NO_SCHEDULE_OVERRIDE_LABEL", prev)
			}
		})
		assert.Equal(t, "backend", loadNoScheduleOverrideLabel())
	})

	t.Run("reads and trims an explicit override", func(t *testing.T) {
		t.Setenv("WOODPECKER_NO_SCHEDULE_OVERRIDE_LABEL", "  worker-class  ")
		assert.Equal(t, "worker-class", loadNoScheduleOverrideLabel())
	})

	t.Run("empty string disables the override (distinct from unset)", func(t *testing.T) {
		t.Setenv("WOODPECKER_NO_SCHEDULE_OVERRIDE_LABEL", "")
		assert.Empty(t, loadNoScheduleOverrideLabel())
	})
}

func TestRPCNext_NoSchedule(t *testing.T) {
	t.Run("BlocksGeneralTask: NoSchedule agent with no override label never reaches the queue (#305)", func(t *testing.T) {
		store := store_mocks.NewMockStore(t)
		agent := &model.Agent{ID: 1, NoSchedule: true} // no CustomLabels at all
		store.On("AgentFind", int64(1)).Once().Return(agent, nil)

		s := RPC{store: store, noScheduleOverrideLabel: "backend"}
		// queue mock is intentionally NOT given a Poll expectation — if Next()
		// ever calls Poll, the mock's unmet-expectation assertion (t.Cleanup)
		// fails the test, proving the early-return fast path held.
		s.queue = queue_mocks.NewMockQueue(t)

		wf, err := s.Next(nextTestCtx(t, 1), rpc.Filter{Labels: map[string]string{}})
		require.NoError(t, err)
		assert.Nil(t, wf)
	})

	t.Run("AllowsExplicitlyTargetedTask: a task pinned via the override label matches despite NoSchedule (#305)", func(t *testing.T) {
		store := store_mocks.NewMockStore(t)
		agent := &model.Agent{ID: 2, NoSchedule: true, CustomLabels: map[string]string{"backend": "local-d3ci42"}}
		store.On("AgentFind", int64(2)).Once().Return(agent, nil)

		q := queue_mocks.NewMockQueue(t)
		var captured queue.FilterFn
		q.EXPECT().Poll(mock.Anything, int64(2), mock.Anything).
			Run(func(_ context.Context, _ int64, f queue.FilterFn) { captured = f }).
			Return(nil, nil).Once()

		s := RPC{store: store, queue: q, noScheduleOverrideLabel: "backend"}

		_, err := s.Next(nextTestCtx(t, 2), rpc.Filter{Labels: map[string]string{}})
		require.NoError(t, err)
		require.NotNil(t, captured, "filterFn should have been passed to queue.Poll")

		ok, _ := captured(&model.Task{Labels: map[string]string{"backend": "local-d3ci42"}})
		assert.True(t, ok, "a task explicitly pinned to this agent's backend must match")
	})

	t.Run("cross-backend mismatch: a task pinned to a DIFFERENT backend still does not match (#305)", func(t *testing.T) {
		store := store_mocks.NewMockStore(t)
		agent := &model.Agent{ID: 3, NoSchedule: true, CustomLabels: map[string]string{"backend": "local-d3ci42"}}
		store.On("AgentFind", int64(3)).Once().Return(agent, nil)

		q := queue_mocks.NewMockQueue(t)
		var captured queue.FilterFn
		q.EXPECT().Poll(mock.Anything, int64(3), mock.Anything).
			Run(func(_ context.Context, _ int64, f queue.FilterFn) { captured = f }).
			Return(nil, nil).Once()

		s := RPC{store: store, queue: q, noScheduleOverrideLabel: "backend"}

		_, err := s.Next(nextTestCtx(t, 3), rpc.Filter{Labels: map[string]string{}})
		require.NoError(t, err)
		require.NotNil(t, captured)

		ok, _ := captured(&model.Task{Labels: map[string]string{"backend": "some-other-backend"}})
		assert.False(t, ok, "a task pinned to a different backend must not match")
	})

	t.Run("untargeted task (no backend label at all) still does not match a NoSchedule agent (#305)", func(t *testing.T) {
		store := store_mocks.NewMockStore(t)
		agent := &model.Agent{ID: 4, NoSchedule: true, CustomLabels: map[string]string{"backend": "local-d3ci42"}}
		store.On("AgentFind", int64(4)).Once().Return(agent, nil)

		q := queue_mocks.NewMockQueue(t)
		var captured queue.FilterFn
		q.EXPECT().Poll(mock.Anything, int64(4), mock.Anything).
			Run(func(_ context.Context, _ int64, f queue.FilterFn) { captured = f }).
			Return(nil, nil).Once()

		s := RPC{store: store, queue: q, noScheduleOverrideLabel: "backend"}

		_, err := s.Next(nextTestCtx(t, 4), rpc.Filter{Labels: map[string]string{}})
		require.NoError(t, err)
		require.NotNil(t, captured)

		ok, _ := captured(&model.Task{Labels: map[string]string{}})
		assert.False(t, ok, "a general task with no backend requirement must not match a cordoned agent")
	})

	t.Run("empty noScheduleOverrideLabel disables the override entirely (restores pre-#305 behavior)", func(t *testing.T) {
		store := store_mocks.NewMockStore(t)
		agent := &model.Agent{ID: 5, NoSchedule: true, CustomLabels: map[string]string{"backend": "local-d3ci42"}}
		store.On("AgentFind", int64(5)).Once().Return(agent, nil)

		s := RPC{store: store, noScheduleOverrideLabel: ""}
		s.queue = queue_mocks.NewMockQueue(t) // no Poll expectation — must not be called

		wf, err := s.Next(nextTestCtx(t, 5), rpc.Filter{Labels: map[string]string{}})
		require.NoError(t, err)
		assert.Nil(t, wf)
	})

	t.Run("a non-NoSchedule agent is unaffected by the override plumbing", func(t *testing.T) {
		store := store_mocks.NewMockStore(t)
		agent := &model.Agent{ID: 6, NoSchedule: false, CustomLabels: map[string]string{"backend": "some-agent"}}
		store.On("AgentFind", int64(6)).Once().Return(agent, nil)

		q := queue_mocks.NewMockQueue(t)
		q.EXPECT().Poll(mock.Anything, int64(6), mock.Anything).Return(nil, nil).Once()

		s := RPC{store: store, queue: q, noScheduleOverrideLabel: "backend"}

		_, err := s.Next(nextTestCtx(t, 6), rpc.Filter{Labels: map[string]string{}})
		require.NoError(t, err)
	})
}
