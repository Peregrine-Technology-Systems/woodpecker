// Copyright 2026 Peregrine Technology Systems
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

package logdrain

import (
	"errors"
	"testing"
	"time"

	"cloud.google.com/go/logging"
	"github.com/stretchr/testify/assert"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
)

type mockLogger struct {
	entries []logging.Entry
}

func (m *mockLogger) Log(e logging.Entry) { m.entries = append(m.entries, e) }

func TestEnabled(t *testing.T) {
	var nilDrain *Drain
	assert.False(t, nilDrain.Enabled(), "nil drain is disabled")
	assert.False(t, (&Drain{}).Enabled(), "zero drain is disabled")
	assert.True(t, newDrain(&mockLogger{}, "log", nil).Enabled(), "drain with a logger is enabled")
}

func TestAppendForwardsEachLine(t *testing.T) {
	m := &mockLogger{}
	d := newDrain(m, "projects/p/logs/woodpecker-steps", nil)
	step := &model.Step{ID: 7, Name: "deploy", State: model.StatusRunning, Started: 1_000_000}

	d.Append("org/repo", 42, step, []*model.LogEntry{
		{Time: 0, Line: 0, Data: []byte("starting")},
		{Time: 2, Line: 1, Data: []byte("done")},
		nil, // nil entry is skipped
	})

	assert.Len(t, m.entries, 2)
	e0 := m.entries[0]
	assert.Equal(t, "projects/p/logs/woodpecker-steps", e0.LogName)
	assert.Equal(t, "org/repo", e0.Labels["repo"])
	assert.Equal(t, "42", e0.Labels["pipeline"])
	assert.Equal(t, "deploy", e0.Labels["step"])
	assert.Equal(t, "7", e0.Labels["step_id"])
	assert.Equal(t, "running", e0.Labels["status"])
	assert.Equal(t, "starting", e0.Payload)
	assert.Equal(t, logging.Info, e0.Severity)
	assert.Equal(t, "generic_task", e0.Resource.Type)
	// wall-clock reconstructed from step.Started + offset
	assert.Equal(t, time.Unix(1_000_000, 0).UTC(), e0.Timestamp)
	assert.Equal(t, time.Unix(1_000_002, 0).UTC(), m.entries[1].Timestamp)
}

func TestAppendDisabledIsNoOp(t *testing.T) {
	var d *Drain // nil → disabled
	assert.NotPanics(t, func() {
		d.Append("org/repo", 1, &model.Step{}, []*model.LogEntry{{Data: []byte("x")}})
	})

	zero := &Drain{}
	zero.Append("org/repo", 1, &model.Step{}, []*model.LogEntry{{Data: []byte("x")}})
	// nothing to assert beyond not panicking + no logger to receive
}

func TestAppendNilStepIsNoOp(t *testing.T) {
	m := &mockLogger{}
	d := newDrain(m, "log", nil)
	d.Append("org/repo", 1, nil, []*model.LogEntry{{Data: []byte("x")}})
	assert.Empty(t, m.entries)
}

func TestBuildEntryZeroStartUsesIngestionTime(t *testing.T) {
	// step.Started == 0 → leave Timestamp zero so the GCP client stamps now.
	step := &model.Step{ID: 1, Name: "x", State: model.StatusSuccess, Started: 0}
	e := buildEntry("log", "org/repo", 1, step, &model.LogEntry{Time: 5, Data: []byte("hi")})
	assert.True(t, e.Timestamp.IsZero(), "zero start → zero timestamp (ingestion time)")
	assert.Equal(t, logging.Info, e.Severity)
}

func TestSeverityFromStatus(t *testing.T) {
	cases := map[model.StatusValue]logging.Severity{
		model.StatusRunning: logging.Info,
		model.StatusPending: logging.Info,
		model.StatusSuccess: logging.Info,
		model.StatusFailure: logging.Error,
		model.StatusError:   logging.Error,
		model.StatusKilled:  logging.Error,
		model.StatusSkipped: logging.Notice,
	}
	for status, want := range cases {
		assert.Equal(t, want, severityFromStatus(status), "severity for %s", status)
	}
}

func TestClose(t *testing.T) {
	var nilDrain *Drain
	assert.NoError(t, nilDrain.Close(), "nil drain close is a no-op")
	assert.NoError(t, (&Drain{}).Close(), "disabled drain close is a no-op")

	called := false
	d := newDrain(&mockLogger{}, "log", func() error { called = true; return nil })
	assert.NoError(t, d.Close())
	assert.True(t, called, "closeFn invoked")

	boom := errors.New("flush failed")
	d2 := newDrain(&mockLogger{}, "log", func() error { return boom })
	assert.ErrorIs(t, d2.Close(), boom, "close surfaces the client error")
}
