// Copyright 2021 Woodpecker Authors
// Copyright 2018 Drone.IO Inc.
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

import "strings"

// Workflow represents a workflow in the pipeline.
type Workflow struct {
	ID         int64             `json:"id"                   xorm:"pk autoincr 'id'"`
	PipelineID int64             `json:"pipeline_id"          xorm:"UNIQUE(s) INDEX 'pipeline_id'"`
	PID        int               `json:"pid"                  xorm:"UNIQUE(s) 'pid'"`
	Name       string            `json:"name"                 xorm:"name"`
	State      StatusValue       `json:"state"                xorm:"state"`
	Error      string            `json:"error,omitempty"      xorm:"TEXT 'error'"`
	Started    int64             `json:"started,omitempty"    xorm:"started"`
	Finished   int64             `json:"finished,omitempty"   xorm:"finished"`
	AgentID    int64             `json:"agent_id,omitempty"   xorm:"agent_id"`
	Platform   string            `json:"platform,omitempty"   xorm:"platform"`
	Environ    map[string]string `json:"environ,omitempty"    xorm:"json 'environ'"`
	DependsOn  []string          `json:"depends_on,omitempty" xorm:"json 'depends_on'"`
	AxisID     int               `json:"-"                    xorm:"axis_id"`
	Children   []*Step           `json:"children,omitempty"   xorm:"-"`
}

// TableName return database table name for xorm.
func (Workflow) TableName() string {
	return "workflows"
}

// Running returns true if the process state is pending or running.
func (p *Workflow) Running() bool {
	return p.State == StatusPending || p.State == StatusRunning
}

// Failing returns true if the process state is failed, killed or error.
func (p *Workflow) Failing() bool {
	return p.State == StatusError || p.State == StatusKilled || p.State == StatusSuperseded || p.State == StatusFailure
}

// disconnectErrorSignatures matches workflow.Error strings produced by the
// agent-disconnect kill paths in server/rpc/rpc.go (releaseAgentTasks and the
// queue.Done propagation). These are infrastructure failures (spot VM
// preemption, MIG resize-while-working, network partition) — they say nothing
// about whether the SHA is mergeable. See KilledByAgentDisconnect.
var disconnectErrorSignatures = []string{
	"agent disconnected",
}

// KilledByAgentDisconnect returns true when the workflow was killed because
// the agent running it disconnected (spot VM preemption, MIG resize, network
// partition). Used to suppress forge.Status posts for these workflows so the
// resulting "errored" check does not cascade into branch-protection blocks
// (fork#44). The conservative read is: workflow is in StatusKilled AND its
// Error field carries a known disconnect signature. Real failures (exit code
// != 0, config errors, manual kills with explicit reasons) still post.
func (p *Workflow) KilledByAgentDisconnect() bool {
	if p == nil {
		return false
	}
	if p.State != StatusKilled {
		return false
	}
	for _, sig := range disconnectErrorSignatures {
		if strings.Contains(p.Error, sig) {
			return true
		}
	}
	return false
}

// IsThereRunningStage determine if it contains workflows running or pending to run.
// TODO: return false based on depends_on (https://github.com/woodpecker-ci/woodpecker/pull/730#discussion_r795681697)
func IsThereRunningStage(workflows []*Workflow) bool {
	for _, p := range workflows {
		if p.Running() {
			return true
		}
	}
	return false
}
