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

import "testing"

// #382: a dependent whose agent never registers is never polled, so ShouldRun
// is never evaluated and the task sits pending forever. CanNeverRun is the
// agent-independent form of that decision, so the server can skip such a task
// without any agent asking for it.
func TestTaskCanNeverRun(t *testing.T) {
	tests := []struct {
		name string
		task *Task
		want bool
	}{
		{
			name: "no dependencies is never unrunnable",
			task: &Task{ID: "1"},
			want: false,
		},
		{
			name: "dep succeeded and task runs on success",
			task: &Task{ID: "2", Dependencies: []string{"a"}, DepStatus: map[string]StatusValue{"a": StatusSuccess}},
			want: false,
		},
		{
			// The #382 case: pts-build-compile depends on a failed wake.
			name: "dep failed and task only runs on success",
			task: &Task{ID: "3", Dependencies: []string{"a"}, DepStatus: map[string]StatusValue{"a": StatusFailure}},
			want: true,
		},
		{
			// Criterion 3: when:status:[success,failure] must still run.
			name: "dep failed but task also runs on failure",
			task: &Task{
				ID: "4", Dependencies: []string{"a"},
				DepStatus: map[string]StatusValue{"a": StatusFailure},
				RunOn:     []string{"success", "failure"},
			},
			want: false,
		},
		{
			name: "dep succeeded but task only runs on failure",
			task: &Task{
				ID: "5", Dependencies: []string{"a"},
				DepStatus: map[string]StatusValue{"a": StatusSuccess},
				RunOn:     []string{"failure"},
			},
			want: true,
		},
		{
			name: "dep status not yet reported is not terminal",
			task: &Task{ID: "6", Dependencies: []string{"a"}, DepStatus: map[string]StatusValue{}},
			want: false,
		},
		{
			name: "dep still running is not terminal",
			task: &Task{ID: "7", Dependencies: []string{"a"}, DepStatus: map[string]StatusValue{"a": StatusRunning}},
			want: false,
		},
		{
			name: "dep pending is not terminal",
			task: &Task{ID: "8", Dependencies: []string{"a"}, DepStatus: map[string]StatusValue{"a": StatusPending}},
			want: false,
		},
		{
			name: "dep blocked on approval is not terminal",
			task: &Task{ID: "9", Dependencies: []string{"a"}, DepStatus: map[string]StatusValue{"a": StatusBlocked}},
			want: false,
		},
		{
			// Fail-safe: skipping is the destructive act, so an unrecognised
			// status must never license it.
			name: "unknown dep status is treated as non-terminal",
			task: &Task{ID: "10", Dependencies: []string{"a"}, DepStatus: map[string]StatusValue{"a": StatusValue("banana")}},
			want: false,
		},
		{
			name: "one dep terminal one unreported waits for the second",
			task: &Task{
				ID: "11", Dependencies: []string{"a", "b"},
				DepStatus: map[string]StatusValue{"a": StatusFailure},
			},
			want: false,
		},
		{
			name: "both deps terminal and one failed",
			task: &Task{
				ID: "12", Dependencies: []string{"a", "b"},
				DepStatus: map[string]StatusValue{"a": StatusFailure, "b": StatusSuccess},
			},
			want: true,
		},
		{
			name: "killed dep is terminal",
			task: &Task{ID: "13", Dependencies: []string{"a"}, DepStatus: map[string]StatusValue{"a": StatusKilled}},
			want: true,
		},
		{
			name: "skipped dep is terminal",
			task: &Task{ID: "14", Dependencies: []string{"a"}, DepStatus: map[string]StatusValue{"a": StatusSkipped}},
			want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.task.CanNeverRun(); got != tc.want {
				t.Errorf("CanNeverRun() = %v, want %v", got, tc.want)
			}
		})
	}
}

// CanNeverRun must never contradict ShouldRun: if a task can never run, then
// ShouldRun must already be false for it. Guards against the two drifting.
func TestTaskCanNeverRunImpliesShouldNotRun(t *testing.T) {
	statuses := []StatusValue{
		StatusSuccess, StatusFailure, StatusKilled, StatusError,
		StatusCanceled, StatusSkipped, StatusSuperseded,
		StatusPending, StatusRunning, StatusBlocked,
	}
	runOns := [][]string{nil, {"success"}, {"failure"}, {"success", "failure"}}

	for _, st := range statuses {
		for _, ro := range runOns {
			task := &Task{
				ID:           "x",
				Dependencies: []string{"a"},
				DepStatus:    map[string]StatusValue{"a": st},
				RunOn:        ro,
			}
			if task.CanNeverRun() && task.ShouldRun() {
				t.Errorf("status=%s run_on=%v: CanNeverRun and ShouldRun both true", st, ro)
			}
		}
	}
}
