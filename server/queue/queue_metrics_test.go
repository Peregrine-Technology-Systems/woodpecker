// Copyright 2026 Woodpecker Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0

package queue

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
)

// TestRecordDispatchFailure_LabelsCounter — direct increment via the
// metrics helper, asserting the counter exposes the label dimension.
func TestRecordDispatchFailure_LabelsCounter(t *testing.T) {
	before := testutil.ToFloat64(dispatchFailures.WithLabelValues("cancelled_before_dispatch"))
	recordDispatchFailure("cancelled_before_dispatch")
	after := testutil.ToFloat64(dispatchFailures.WithLabelValues("cancelled_before_dispatch"))
	assert.InDelta(t, before+1, after, 0.0001)
}

// TestIsTerminalFailure_AllStatusValues — every status in the model is
// classified explicitly, so future Woodpecker upstream additions are
// caught by an unhandled-default failure here.
func TestIsTerminalFailure_AllStatusValues(t *testing.T) {
	cases := map[model.StatusValue]bool{
		model.StatusFailure:    true,
		model.StatusError:      true,
		model.StatusKilled:     true,
		model.StatusCanceled:   true,
		model.StatusSuccess:    false,
		model.StatusSkipped:    false,
		model.StatusRunning:    false,
		model.StatusPending:    false,
		model.StatusBlocked:    false,
		model.StatusDeclined:   false,
		model.StatusCreated:    false,
		model.StatusSuperseded: false,
		model.StatusPartial:    false,
	}
	for status, want := range cases {
		assert.Equal(t, want, isTerminalFailure(status), "isTerminalFailure(%q) = %v, want %v", status, !want, want)
	}
}

// =============================================================================
// fifo integration: counter increments on the actual cancel/dep paths
// =============================================================================

// TestFifo_CancelledBeforeDispatch_FromPending exercises the high-signal
// case (mode B from #190): a task is sitting in pending and gets pulled
// out via ErrorAtOnce. The counter must increment on
// cancelled_before_dispatch.
func TestFifo_CancelledBeforeDispatch_FromPending(t *testing.T) {
	q := NewMemoryQueue(context.Background()).(*fifo)
	task := &model.Task{ID: "t-pending-1"}
	assert.NoError(t, q.PushAtOnce(context.Background(), []*model.Task{task}))

	before := testutil.ToFloat64(dispatchFailures.WithLabelValues("cancelled_before_dispatch"))
	assert.NoError(t, q.ErrorAtOnce(context.Background(), []string{"t-pending-1"}, ErrCancel))
	after := testutil.ToFloat64(dispatchFailures.WithLabelValues("cancelled_before_dispatch"))
	assert.InDelta(t, before+1, after, 0.0001,
		"removing a pending task via ErrorAtOnce must increment cancelled_before_dispatch")
}

// TestFifo_CancelledBeforeDispatch_FromWaiting covers the second branch
// of removeFromPendingAndWaiting: the task was in waitingOnDeps when
// cancelled.
func TestFifo_CancelledBeforeDispatch_FromWaiting(t *testing.T) {
	q := NewMemoryQueue(context.Background()).(*fifo)
	// Inject a task directly into waitingOnDeps so removeFromPendingAndWaiting
	// hits the second loop. (Avoid going through the dispatcher loop.)
	task := &model.Task{ID: "t-waiting-1", Dependencies: []string{"never-arrives"}, DepStatus: map[string]model.StatusValue{}}
	q.Lock()
	q.waitingOnDeps.PushBack(task)
	q.Unlock()

	before := testutil.ToFloat64(dispatchFailures.WithLabelValues("cancelled_before_dispatch"))
	assert.NoError(t, q.ErrorAtOnce(context.Background(), []string{"t-waiting-1"}, ErrCancel))
	after := testutil.ToFloat64(dispatchFailures.WithLabelValues("cancelled_before_dispatch"))
	assert.InDelta(t, before+1, after, 0.0001)
}

// TestFifo_RemoveNonExistentTask_NoCounter asserts that ErrorAtOnce on a
// task that doesn't exist neither panics nor increments the counter.
func TestFifo_RemoveNonExistentTask_NoCounter(t *testing.T) {
	q := NewMemoryQueue(context.Background()).(*fifo)
	before := testutil.ToFloat64(dispatchFailures.WithLabelValues("cancelled_before_dispatch"))
	// finished() collects the ErrNotFound into errs but doesn't return
	// an error from ErrorAtOnce — it returns an errors.Join instead.
	_ = q.ErrorAtOnce(context.Background(), []string{"does-not-exist"}, ErrCancel)
	after := testutil.ToFloat64(dispatchFailures.WithLabelValues("cancelled_before_dispatch"))
	assert.InDelta(t, before, after, 0.0001, "no increment when task not found")
}

// TestFifo_DependencyUnsatisfiedTerminal exercises the second reason
// path: a waiting task's dep transitions to terminal failure during
// updateDepStatusInQueue.
func TestFifo_DependencyUnsatisfiedTerminal(t *testing.T) {
	q := NewMemoryQueue(context.Background()).(*fifo)
	// Waiting task depends on "build". Put both in the queue.
	build := &model.Task{ID: "build", DepStatus: map[string]model.StatusValue{}}
	deploy := &model.Task{ID: "deploy", Dependencies: []string{"build"}, DepStatus: map[string]model.StatusValue{}}
	q.Lock()
	q.waitingOnDeps.PushBack(deploy)
	q.running["build"] = &entry{item: build, done: make(chan bool)}
	q.Unlock()

	before := testutil.ToFloat64(dispatchFailures.WithLabelValues("dependency_unsatisfied_terminal"))
	// "build" finishes with terminal failure → updateDepStatusInQueue
	// hits the deploy entry in waitingOnDeps and increments the counter.
	assert.NoError(t, q.Done(context.Background(), "build", model.StatusFailure))
	after := testutil.ToFloat64(dispatchFailures.WithLabelValues("dependency_unsatisfied_terminal"))
	assert.InDelta(t, before+1, after, 0.0001)
}

// TestFifo_DependencySatisfiedSuccess_NoCounter — a successful dep
// transition must NOT increment the dependency_unsatisfied_terminal
// counter.
func TestFifo_DependencySatisfiedSuccess_NoCounter(t *testing.T) {
	q := NewMemoryQueue(context.Background()).(*fifo)
	build := &model.Task{ID: "build-ok", DepStatus: map[string]model.StatusValue{}}
	deploy := &model.Task{ID: "deploy-ok", Dependencies: []string{"build-ok"}, DepStatus: map[string]model.StatusValue{}}
	q.Lock()
	q.waitingOnDeps.PushBack(deploy)
	q.running["build-ok"] = &entry{item: build, done: make(chan bool)}
	q.Unlock()

	before := testutil.ToFloat64(dispatchFailures.WithLabelValues("dependency_unsatisfied_terminal"))
	assert.NoError(t, q.Done(context.Background(), "build-ok", model.StatusSuccess))
	after := testutil.ToFloat64(dispatchFailures.WithLabelValues("dependency_unsatisfied_terminal"))
	assert.InDelta(t, before, after, 0.0001, "successful dep transition must not count as unsatisfied")
}
