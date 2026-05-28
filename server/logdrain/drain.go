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

// Package logdrain forwards step log lines to GCP Cloud Logging in parallel
// with the SQLite write, so CI step output is queryable in GCP Log Explorer
// without a Woodpecker UI account (#233). The drain is strictly best-effort:
// SQLite remains the source of truth, and a drain failure never affects the
// store write or the agent's gRPC ack.
package logdrain

import (
	"strconv"
	"time"

	"cloud.google.com/go/logging"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
)

// entryLogger is the slice of the GCP *logging.Logger the drain uses. An
// interface so the drain is unit-testable with a mock. Logger.Log is
// asynchronous (the client buffers, batches, and retries internally), so
// Append never blocks the caller.
type entryLogger interface {
	Log(e logging.Entry)
}

// Drain forwards step log lines to a Cloud Logging log. The zero value (and a
// nil *Drain) is a disabled no-op, so callers can use it unconditionally.
type Drain struct {
	logger  entryLogger
	logName string
	closeFn func() error
}

// newDrain builds a Drain around an entryLogger — the testable core, used by
// New (real client) and by tests (mock).
func newDrain(logger entryLogger, logName string, closeFn func() error) *Drain {
	return &Drain{logger: logger, logName: logName, closeFn: closeFn}
}

// Enabled reports whether the drain will forward anything.
func (d *Drain) Enabled() bool {
	return d != nil && d.logger != nil
}

// Append forwards a batch of step log lines to Cloud Logging. No-op when
// disabled. Best-effort: the async client surfaces transport errors to its
// own error handler (logged at WARN), never to this caller.
func (d *Drain) Append(repoFullName string, pipelineNumber int64, step *model.Step, entries []*model.LogEntry) {
	if !d.Enabled() || step == nil {
		return
	}
	for _, e := range entries {
		if e == nil {
			continue
		}
		d.logger.Log(buildEntry(d.logName, repoFullName, pipelineNumber, step, e))
	}
}

// Close flushes and closes the underlying client. Safe on a nil/disabled drain.
func (d *Drain) Close() error {
	if d == nil || d.closeFn == nil {
		return nil
	}
	return d.closeFn()
}

// buildEntry maps a single step log line to a Cloud Logging entry.
//
// Timestamp: model.LogEntry.Time is the elapsed seconds since the step started
// (see agent/log/line_writer.go), NOT a wall-clock value, so it is reconstructed
// as step.Started + offset. When the step has no recorded start (Started == 0),
// the Timestamp is left zero and the GCP client stamps ingestion time.
func buildEntry(logName, repoFullName string, pipelineNumber int64, step *model.Step, e *model.LogEntry) logging.Entry {
	var ts time.Time
	if step.Started > 0 {
		ts = time.Unix(step.Started+e.Time, 0).UTC()
	}
	return logging.Entry{
		LogName: logName,
		Resource: &mrpb.MonitoredResource{
			Type:   "generic_task",
			Labels: map[string]string{"job": "woodpecker-steps"},
		},
		Labels: map[string]string{
			"repo":     repoFullName,
			"pipeline": strconv.FormatInt(pipelineNumber, 10),
			"step":     step.Name,
			"step_id":  strconv.FormatInt(step.ID, 10),
			"status":   string(step.State),
		},
		Payload:   string(e.Data),
		Timestamp: ts,
		Severity:  severityFromStatus(step.State),
	}
}

// severityFromStatus maps a step's state to a Cloud Logging severity.
func severityFromStatus(s model.StatusValue) logging.Severity {
	switch s {
	case model.StatusFailure, model.StatusError, model.StatusKilled:
		return logging.Error
	case model.StatusSkipped:
		return logging.Notice
	default: // pending, running, success, …
		return logging.Info
	}
}
