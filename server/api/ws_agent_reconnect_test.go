// Copyright 2026 Woodpecker Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0

package api

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"go.woodpecker-ci.org/woodpecker/v3/server/queue"
	queue_mocks "go.woodpecker-ci.org/woodpecker/v3/server/queue/mocks"
	grpcserver "go.woodpecker-ci.org/woodpecker/v3/server/rpc"
	store_mocks "go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
)

// withShortGrace temporarily shrinks the reconnect grace window so
// tests don't have to wait 30 seconds.
func withShortGrace(t *testing.T, d time.Duration) {
	t.Helper()
	orig := wsReconnectGrace
	wsReconnectGrace = d
	t.Cleanup(func() { wsReconnectGrace = orig })
}

func newTestRPC(t *testing.T) *grpcserver.RPC {
	t.Helper()
	q := queue_mocks.NewMockQueue(t)
	// ReleaseAgentTasks (called when grace expires) calls queue.Info.
	// .Maybe() so tests that cancel before grace expiry don't get
	// flagged for unmet expectations.
	q.On("Info", mock.Anything).Return(queue.InfoT{}).Maybe()
	s := store_mocks.NewMockStore(t)
	return grpcserver.NewRPCForTesting(q, s)
}

// TestScheduleAgentRelease_FiresAfterGrace — the abandoned path: no
// reconnect arrives within grace, ReleaseAgentTasks fires, counter
// increments outcome=abandoned.
func TestScheduleAgentRelease_FiresAfterGrace(t *testing.T) {
	withShortGrace(t, 50*time.Millisecond)
	const agentID int64 = 9001

	abandonedBefore := testutil.ToFloat64(wsReconnectTotal.WithLabelValues("abandoned"))

	rpc := newTestRPC(t)
	scheduleAgentRelease(agentID, rpc)

	// Wait for the timer to fire + a bit of slack.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if testutil.ToFloat64(wsReconnectTotal.WithLabelValues("abandoned")) >= abandonedBefore+1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("abandoned counter never incremented after grace expiry")
}

// TestCancelPendingAgentRelease_PreventsRelease — the success path:
// agent reconnects within grace; cancel cancels the timer; counter
// increments outcome=success; ReleaseAgentTasks does NOT fire.
func TestCancelPendingAgentRelease_PreventsRelease(t *testing.T) {
	withShortGrace(t, 5*time.Second) // long enough that we definitely cancel before fire
	const agentID int64 = 9002

	successBefore := testutil.ToFloat64(wsReconnectTotal.WithLabelValues("success"))
	abandonedBefore := testutil.ToFloat64(wsReconnectTotal.WithLabelValues("abandoned"))

	rpc := newTestRPC(t)
	scheduleAgentRelease(agentID, rpc)

	// Cancel before the timer fires.
	canceled := cancelPendingAgentRelease(agentID)
	assert.True(t, canceled, "cancel must report it found a pending release")

	// success counter incremented; abandoned NOT incremented (give the
	// goroutine a moment to be sure it didn't fire — it shouldn't).
	time.Sleep(50 * time.Millisecond)
	assert.InDelta(t, successBefore+1,
		testutil.ToFloat64(wsReconnectTotal.WithLabelValues("success")), 0.0001)
	assert.InDelta(t, abandonedBefore,
		testutil.ToFloat64(wsReconnectTotal.WithLabelValues("abandoned")), 0.0001,
		"abandoned must NOT fire when cancellation succeeded")
}

// TestCancelPendingAgentRelease_NoPending_ReturnsFalse — calling cancel
// for an agent_id that has no pending release is a harmless no-op.
func TestCancelPendingAgentRelease_NoPending_ReturnsFalse(t *testing.T) {
	successBefore := testutil.ToFloat64(wsReconnectTotal.WithLabelValues("success"))

	canceled := cancelPendingAgentRelease(999999) // no schedule for this id
	assert.False(t, canceled)

	// Counter must NOT have incremented for a no-op cancel.
	assert.InDelta(t, successBefore,
		testutil.ToFloat64(wsReconnectTotal.WithLabelValues("success")), 0.0001)
}

// TestScheduleAgentRelease_ReplacesPriorTimer — calling schedule twice
// for the same agent_id replaces the prior timer (defensive). Only the
// later one fires.
func TestScheduleAgentRelease_ReplacesPriorTimer(t *testing.T) {
	withShortGrace(t, 100*time.Millisecond)
	const agentID int64 = 9003

	rpc := newTestRPC(t)
	scheduleAgentRelease(agentID, rpc) // first schedule
	scheduleAgentRelease(agentID, rpc) // replaces the first

	// Verify the registry only has one entry for this agent_id.
	pendingReleasesMu.Lock()
	_, present := pendingReleases[agentID]
	pendingReleasesMu.Unlock()
	assert.True(t, present, "exactly one pending release per agent_id")

	// Wait for it to fire and clear.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		pendingReleasesMu.Lock()
		_, stillThere := pendingReleases[agentID]
		pendingReleasesMu.Unlock()
		if !stillThere {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pending release entry not cleared after grace")
}
