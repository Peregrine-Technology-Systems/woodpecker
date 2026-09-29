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

package model

// isTerminalStatus reports whether a dependency's status can still change.
//
// Deliberately enumerates the TERMINAL set rather than the non-terminal one, so
// an unrecognised status answers "not terminal". CanNeverRun's caller SKIPS a
// workflow, which is the destructive direction — an unknown status must never
// license it (fail-safe). Adding a new non-terminal status therefore needs no
// change here; adding a new terminal one does, and until it is added the only
// cost is that the skip waits rather than misfires.
func isTerminalStatus(s StatusValue) bool {
	switch s {
	case StatusSuccess, StatusFailure, StatusKilled, StatusError,
		StatusCanceled, StatusSkipped, StatusSuperseded, StatusPartial,
		StatusDeclined:
		return true
	default:
		return false
	}
}

// CanNeverRun reports whether this task's dependencies have all settled AND the
// outcome means the task will never be eligible to run.
//
// This is the agent-independent twin of ShouldRun (#382). ShouldRun is only
// consulted when an agent polls for a task, so a task pinned to a label that no
// agent advertises — e.g. a workflow whose agent an EARLIER workflow in the same
// pipeline was supposed to provision — is never evaluated at all and sits
// pending forever, leaving its pipeline "running" and its own dependents blocked
// behind it. CanNeverRun lets the server reach that verdict with no agent
// involved.
//
// It is strictly more conservative than !ShouldRun(): every declared dependency
// must have reported a terminal status first. A task with one failed dependency
// and one still running is left alone until the second settles, because
// reasoning about run_on from a partially-filled DepStatus is how a task that
// SHOULD run gets skipped. The later settle re-evaluates it.
func (t *Task) CanNeverRun() bool {
	if len(t.Dependencies) == 0 {
		return false
	}
	for _, dep := range t.Dependencies {
		status, reported := t.DepStatus[dep]
		if !reported || !isTerminalStatus(status) {
			return false
		}
	}
	return !t.ShouldRun()
}
