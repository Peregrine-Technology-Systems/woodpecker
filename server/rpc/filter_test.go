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
	"testing"

	"github.com/stretchr/testify/assert"

	"go.woodpecker-ci.org/woodpecker/v3/rpc"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
)

func TestCreateFilterFunc(t *testing.T) {
	tests := []struct {
		name        string
		agentFilter rpc.Filter
		task        *model.Task
		wantMatched bool
		wantScore   int
	}{
		{
			name: "Two exact matches",
			agentFilter: rpc.Filter{
				Labels: map[string]string{"org-id": "123", "platform": "linux"},
			},
			task: &model.Task{
				Labels: map[string]string{"org-id": "123", "platform": "linux"},
			},
			wantMatched: true,
			wantScore:   20,
		},
		{
			name: "Wildcard and exact match",
			agentFilter: rpc.Filter{
				Labels: map[string]string{"org-id": "*", "platform": "linux"},
			},
			task: &model.Task{
				Labels: map[string]string{"org-id": "123", "platform": "linux"},
			},
			wantMatched: true,
			wantScore:   11,
		},
		{
			name: "Partial match",
			agentFilter: rpc.Filter{
				Labels: map[string]string{"org-id": "123", "platform": "linux"},
			},
			task: &model.Task{
				Labels: map[string]string{"org-id": "123", "platform": "windows"},
			},
			wantMatched: false,
			wantScore:   0,
		},
		{
			name: "No match",
			agentFilter: rpc.Filter{
				Labels: map[string]string{"org-id": "456", "platform": "linux"},
			},
			task: &model.Task{
				Labels: map[string]string{"org-id": "123", "platform": "windows"},
			},
			wantMatched: false,
			wantScore:   0,
		},
		{
			name: "Missing label",
			agentFilter: rpc.Filter{
				Labels: map[string]string{"platform": "linux"},
			},
			task: &model.Task{
				Labels: map[string]string{"needed": "some"},
			},
			wantMatched: false,
			wantScore:   0,
		},
		{
			name: "Empty task labels",
			agentFilter: rpc.Filter{
				Labels: map[string]string{"org-id": "123", "platform": "linux"},
			},
			task: &model.Task{
				Labels: map[string]string{},
			},
			wantMatched: true,
			wantScore:   0,
		},
		{
			name: "Agent with additional label",
			agentFilter: rpc.Filter{
				Labels: map[string]string{"org-id": "123", "platform": "linux", "extra": "value"},
			},
			task: &model.Task{
				Labels: map[string]string{"org-id": "123", "platform": "linux", "empty": ""},
			},
			wantMatched: true,
			wantScore:   20,
		},
		{
			name: "Two wildcard matches",
			agentFilter: rpc.Filter{
				Labels: map[string]string{"org-id": "*", "platform": "*"},
			},
			task: &model.Task{
				Labels: map[string]string{"org-id": "123", "platform": "linux"},
			},
			wantMatched: true,
			wantScore:   2,
		},
		{
			name: "Required label matches without shebang",
			agentFilter: rpc.Filter{
				Labels: map[string]string{"!org-id": "123", "platform": "linux", "extra": "value"},
			},
			task: &model.Task{
				Labels: map[string]string{"org-id": "123", "platform": "linux", "empty": ""},
			},
			wantMatched: true,
			wantScore:   20,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filterFunc := createFilterFunc(tt.agentFilter)
			gotMatched, gotScore := filterFunc(tt.task)

			assert.Equal(t, tt.wantMatched, gotMatched, "Matched result")
			assert.Equal(t, tt.wantScore, gotScore, "Score")
		})
	}
}

func TestDeployAutoRouting(t *testing.T) {
	patterns := []string{"deploy", "version-bump", "sync-back"}

	deployTask := &model.Task{
		Name:   "deploy",
		Labels: map[string]string{"platform": "linux", "backend": "local"},
	}
	ciTask := &model.Task{
		Name:   "ci",
		Labels: map[string]string{"platform": "linux", "backend": "local"},
	}

	ondemandAgent := rpc.Filter{
		Labels: map[string]string{"platform": "linux", "backend": "local", "tier": "ondemand"},
	}
	spotAgent := rpc.Filter{
		Labels: map[string]string{"platform": "linux", "backend": "local", "tier": "spot"},
	}
	n2Agent := rpc.Filter{
		Labels: map[string]string{"platform": "linux", "backend": "local", "tier": "n2"},
	}

	// Deploy workflow: on-demand agent gets +20 boost
	filterOD := createFilterFuncWithDeploy(ondemandAgent, patterns)
	matched, scoreOD := filterOD(deployTask)
	assert.True(t, matched)

	filterSpot := createFilterFuncWithDeploy(spotAgent, patterns)
	matched, scoreSpot := filterSpot(deployTask)
	assert.True(t, matched)

	assert.Greater(t, scoreOD, scoreSpot, "on-demand should score higher than spot for deploy")

	// N2 agent gets +15 boost — between on-demand and spot
	filterN2 := createFilterFuncWithDeploy(n2Agent, patterns)
	matched, scoreN2 := filterN2(deployTask)
	assert.True(t, matched)
	assert.Greater(t, scoreN2, scoreSpot, "n2 should score higher than spot for deploy")
	assert.Greater(t, scoreOD, scoreN2, "on-demand should score higher than n2 for deploy")

	// CI workflow: no boost, all agents score equally
	_, ciScoreOD := filterOD(ciTask)
	_, ciScoreSpot := filterSpot(ciTask)
	assert.Equal(t, ciScoreOD, ciScoreSpot, "CI workflows should not get tier boost")
}

func TestIsDeployWorkflow(t *testing.T) {
	patterns := []string{"deploy", "version-bump", "sync-back"}

	assert.True(t, isDeployWorkflow("deploy", patterns))
	assert.True(t, isDeployWorkflow("deploy-staging", patterns))
	assert.True(t, isDeployWorkflow("version-bump", patterns))
	assert.True(t, isDeployWorkflow("sync-back", patterns))
	assert.True(t, isDeployWorkflow("Deploy", patterns))
	assert.False(t, isDeployWorkflow("ci", patterns))
	assert.False(t, isDeployWorkflow("test", patterns))
	assert.False(t, isDeployWorkflow("lint", patterns))
	assert.False(t, isDeployWorkflow("", patterns))
	assert.False(t, isDeployWorkflow("deploy", nil))
}

func TestMissingRequiredLabels(t *testing.T) {
	t.Parallel()

	testdata := []struct {
		taskLabels     map[string]string
		requiredLabels map[string]string
		want           bool
	}{
		// Required label present and matches
		{
			taskLabels:     map[string]string{"os": "linux"},
			requiredLabels: map[string]string{"!os": "linux", "platform": "arm64"},
			want:           false,
		},
		// Required label present but does not match
		{
			taskLabels:     map[string]string{"os": "windows"},
			requiredLabels: map[string]string{"!os": "linux", "platform": "amd64"},
			want:           true,
		},
		// Required label missing
		{
			taskLabels:     map[string]string{"arch": "amd64"},
			requiredLabels: map[string]string{"!os": "linux"},
			want:           true,
		},
		// No agent labels
		{
			taskLabels:     map[string]string{"os": "linux"},
			requiredLabels: map[string]string{},
			want:           false,
		},
	}

	for _, tt := range testdata {
		if got := requiredLabelsMissing(tt.taskLabels, tt.requiredLabels); got != tt.want {
			t.Errorf("requiredLabelsMissing(%v, %v) = %v, want %v", tt.taskLabels, tt.requiredLabels, got, tt.want)
		}
	}
}
