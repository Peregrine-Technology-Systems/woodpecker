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
	"container/list"
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
)

// newOrphanTestFifo builds a bare fifo without starting the process() goroutine,
// so sampleOrphans can be driven deterministically. (#243)
func newOrphanTestFifo() *fifo {
	return &fifo{
		workers:        map[*worker]struct{}{},
		running:        map[string]*entry{},
		pending:        list.New(),
		waitingOnDeps:  list.New(),
		reclaimFiredAt: map[int64]time.Time{},
	}
}

func TestSetAgentReclaimFn(t *testing.T) {
	q := newOrphanTestFifo()
	knownDead := func(int64) bool { return true }
	reclaim := func(int64) {}
	q.SetAgentReclaimFn(knownDead, reclaim)
	assert.NotNil(t, q.agentKnownDead)
	assert.NotNil(t, q.reclaimOrphan)
	assert.True(t, q.agentKnownDead(1))
}

func TestSampleOrphans_RunningDeadOwner(t *testing.T) {
	defer orphanedWorkflows.Reset()
	q := newOrphanTestFifo()
	// agent 7 positively known dead; everyone else alive/untracked.
	q.agentKnownDead = func(id int64) bool { return id == 7 }
	q.running["dead"] = &entry{item: &model.Task{ID: "dead", AgentID: 7}}
	q.running["dead2"] = &entry{item: &model.Task{ID: "dead2", AgentID: 7}} // same dead agent — deduped in `due`
	q.running["live"] = &entry{item: &model.Task{ID: "live", AgentID: 9}}
	q.running["hook"] = &entry{item: &model.Task{ID: "hook", AgentID: 0}} // dispatch-hook claim — skipped

	due := q.sampleOrphans(time.Now())

	assert.Equal(t, []int64{7}, due, "the dead-owner agent appears once even when it owns multiple tasks")
	assert.Equal(t, float64(2), testutil.ToFloat64(orphanedWorkflows.WithLabelValues("running_dead_owner")),
		"both tasks on the dead agent are counted")
}

// TestSampleOrphans_UntrackedAgentNotReclaimed is the #246 regression guard:
// an agent that is NOT positively known dead (e.g. a gRPC/local-backend agent
// that never registers through the WS path, so the liveness oracle has no
// record of it) must NEVER be reclaimed, even though it owns running tasks.
// Pre-#246 the oracle keyed on "absent from the connected set," so these agents
// read as dead and their clone step was killed at t+0s, jamming tier:local.
func TestSampleOrphans_UntrackedAgentNotReclaimed(t *testing.T) {
	defer orphanedWorkflows.Reset()
	q := newOrphanTestFifo()
	// knownDead returns false for every agent — the fail-safe default for any
	// agent the WS liveness path never observed disconnecting.
	q.agentKnownDead = func(int64) bool { return false }
	q.running["local"] = &entry{item: &model.Task{ID: "local", AgentID: 23000}}
	q.running["local2"] = &entry{item: &model.Task{ID: "local2", AgentID: 23000}}

	due := q.sampleOrphans(time.Now())

	assert.Empty(t, due, "an untracked (not-known-dead) agent is never reclaimed")
	assert.Empty(t, q.reclaimFiredAt, "no throttle entry is recorded for a live/untracked agent")
	assert.Equal(t, float64(0), testutil.ToFloat64(orphanedWorkflows.WithLabelValues("running_dead_owner")),
		"untracked-agent tasks are not counted as dead-owner")
}

func TestSampleOrphans_NoReclaimFnDisablesReclaim(t *testing.T) {
	q := newOrphanTestFifo() // agentKnownDead nil
	q.running["x"] = &entry{item: &model.Task{ID: "x", AgentID: 7}}
	assert.Nil(t, q.sampleOrphans(time.Now()))
}

func TestSampleOrphans_ReclaimThrottle(t *testing.T) {
	defer orphanedWorkflows.Reset()
	q := newOrphanTestFifo()
	q.agentKnownDead = func(int64) bool { return true }
	q.running["dead"] = &entry{item: &model.Task{ID: "dead", AgentID: 7}}

	t0 := time.Now()
	assert.Equal(t, []int64{7}, q.sampleOrphans(t0), "first observation fires")
	assert.Empty(t, q.sampleOrphans(t0.Add(time.Second)), "within refire interval: throttled")
	assert.Equal(t, []int64{7}, q.sampleOrphans(t0.Add(reclaimRefireInterval+time.Second)),
		"after refire interval: fires again")
}

func TestSampleOrphans_ThrottleEvictsResolvedAgents(t *testing.T) {
	defer orphanedWorkflows.Reset()
	q := newOrphanTestFifo()
	q.agentKnownDead = func(int64) bool { return true }
	q.running["dead"] = &entry{item: &model.Task{ID: "dead", AgentID: 7}}

	t0 := time.Now()
	q.sampleOrphans(t0)
	assert.Contains(t, q.reclaimFiredAt, int64(7))

	// task reclaimed → no longer running; stale throttle entry is dropped.
	delete(q.running, "dead")
	q.sampleOrphans(t0.Add(time.Second))
	assert.NotContains(t, q.reclaimFiredAt, int64(7))
}

func TestSampleOrphans_PendingDispatchableAging(t *testing.T) {
	defer orphanedWorkflows.Reset()
	q := newOrphanTestFifo()
	now := time.Now()
	stale := now.Add(-2 * orphanAgeThreshold).Unix()
	fresh := now.Unix()

	// dispatchable (no deps) + aged out → counts.
	q.pending.PushBack(&model.Task{ID: "old", Created: stale})
	// dispatchable but young → does not count.
	q.pending.PushBack(&model.Task{ID: "young", Created: fresh})
	// aged but NOT dispatchable (a dep failed) → does not count.
	q.pending.PushBack(&model.Task{ID: "blocked", Created: stale, Dependencies: []string{"d"}, DepStatus: map[string]model.StatusValue{"d": model.StatusFailure}})

	q.sampleOrphans(now)
	assert.Equal(t, float64(1), testutil.ToFloat64(orphanedWorkflows.WithLabelValues("pending_dispatchable")))
}

// TestProcessFiresReclaimForOrphanedRunningTask drives the real process()
// goroutine: a running task owned by a disconnected agent is reclaimed within a
// dispatch tick rather than waiting the TaskTimeout lease. (#243)
func TestProcessFiresReclaimForOrphanedRunningTask(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q, _ := NewMemoryQueue(ctx).(*fifo)

	reclaimed := make(chan int64, 4)
	q.SetAgentReclaimFn(
		func(int64) bool { return true }, // owner positively known dead
		func(id int64) { reclaimed <- id },
	)

	q.Lock()
	// Live deadline so resubmitExpiredPipelines doesn't reclaim it first as
	// "expired" — we want the owner-liveness path to be the trigger.
	q.running["wf-1"] = &entry{item: &model.Task{ID: "wf-1", AgentID: 42}, done: make(chan bool), deadline: time.Now().Add(time.Hour)}
	q.Unlock()

	select {
	case got := <-reclaimed:
		assert.Equal(t, int64(42), got)
	case <-time.After(2 * time.Second):
		t.Fatal("reclaim never fired for orphaned running task")
	}
}
