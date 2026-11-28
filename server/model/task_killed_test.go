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

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// #435 part 2: `when: status: killed` is an explicit opt-in to run after a
// dependency was killed. The existing success/failure rows are unchanged: a
// [success, failure] workflow already runs whatever its dependencies did.
func TestTask_ShouldRun_KilledOptIn(t *testing.T) {
	cases := []struct {
		name  string
		runOn []string
		dep   StatusValue
		want  bool
	}{
		{"killed only, dep killed: runs", []string{"killed"}, StatusKilled, true},
		{"killed only, dep succeeded: skipped", []string{"killed"}, StatusSuccess, false},
		{"killed only, dep failed: skipped", []string{"killed"}, StatusFailure, false},
		{"success+killed, dep killed: runs", []string{"success", "killed"}, StatusKilled, true},
		{"success+killed, dep succeeded: runs", []string{"success", "killed"}, StatusSuccess, true},
		{"success+killed, dep failed: skipped", []string{"success", "killed"}, StatusFailure, false},
		{"success only, dep killed: skipped (unchanged)", []string{"success"}, StatusKilled, false},
		{"success+failure, dep killed: runs (unchanged)", []string{"success", "failure"}, StatusKilled, true},
		{"failure only, dep killed: runs (unchanged)", []string{"failure"}, StatusKilled, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := &Task{RunOn: tc.runOn, DepStatus: map[string]StatusValue{"dep": tc.dep}}
			assert.Equal(t, tc.want, task.ShouldRun())
		})
	}
}

func TestTask_RunsOnKilled(t *testing.T) {
	assert.True(t, (&Task{RunOn: []string{"success", "failure", "killed"}}).RunsOnKilled())
	assert.False(t, (&Task{RunOn: []string{"success", "failure"}}).RunsOnKilled())
	assert.False(t, (&Task{}).RunsOnKilled())
}
