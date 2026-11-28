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

package queue

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/shared/constant"
)

// #427: after a restart the server has no evidence a restored task's agent is
// alive, so the task gets a short first lease instead of the full TaskTimeout
// safety net. Its first renewal puts it back on the normal lease.

func restoredDeadline(q *fifo, id string) time.Time {
	q.Lock()
	defer q.Unlock()
	return q.running[id].deadline
}

func TestRestoredTaskGetsAShortFirstLease(t *testing.T) {
	_, _, q := setupTestQueue(t)
	before := time.Now()

	q.adoptRunning([]*model.Task{{ID: "r", AgentID: 7}})

	deadline := restoredDeadline(q, "r")
	assert.WithinDuration(t, before.Add(q.restoredLease), deadline, time.Second)
	assert.True(t, deadline.Before(before.Add(q.extension)), "shorter than the running lease")
}

// The slowest renewal is the gRPC agent's lease loop, every TaskTimeout/3
// (agent/runner.go); WS agents renew on every 10 s health report. A first
// lease shorter than the slowest renewal would release a live agent's work.
func TestRestoredLeaseOutlastsTheSlowestAgentRenewal(t *testing.T) {
	assert.Greater(t, defaultRestoredLease(), constant.TaskTimeout/3)
	assert.Less(t, defaultRestoredLease(), constant.TaskTimeout)
	assert.Equal(t, 6*time.Minute, defaultRestoredLease(), "at the default 15 min TaskTimeout")
	_, _, q := setupTestQueue(t)
	assert.Equal(t, defaultRestoredLease(), q.restoredLease)
}

func TestRenewingARestoredTaskRestoresTheNormalLease(t *testing.T) {
	ctx, _, q := setupTestQueue(t)
	q.adoptRunning([]*model.Task{{ID: "r", AgentID: 7}})

	before := time.Now()
	require.NoError(t, q.Extend(ctx, 7, "r"))

	assert.WithinDuration(t, before.Add(q.extension), restoredDeadline(q, "r"), time.Second)
}

func TestUnrenewedRestoredTaskIsReleasedAtTheShortLease(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	q, _ := NewMemoryQueue(ctx).(*fifo)
	q.extension = time.Hour // the running lease plays no part
	q.restoredLease = 50 * time.Millisecond
	released := make(chan []*model.Task, 1)
	q.SetRestoredExpiryFn(func(tasks []*model.Task) { released <- tasks })

	q.adoptRunning([]*model.Task{{ID: "r", AgentID: 7}})

	select {
	case got := <-released:
		require.Len(t, got, 1)
		assert.Equal(t, "r", got[0].ID)
	case <-time.After(3 * time.Second):
		t.Fatal("an unrenewed restored task must be released at the short lease, not the running one")
	}
}
