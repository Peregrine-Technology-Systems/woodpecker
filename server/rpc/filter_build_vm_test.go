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

package grpc

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.woodpecker-ci.org/woodpecker/v3/rpc"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
)

// #435: the labels a build VM's agent filters with, as the agent builds them
// (cmd/agent/core/agent.go defaults, then its custom labels) and the server
// adds org-id. tier comes from the VM's ci-tier metadata.
func buildVMFilter(tier string) rpc.Filter {
	return rpc.Filter{Labels: map[string]string{
		"hostname": "pts-build-vm.us-central1-a.c.peregrine-production.internal",
		"platform": "linux", "backend": "local", "repo": "*", "org-id": "*",
		"agent": "pts-build", "cloud": "gcp", "tier": tier,
	}}
}

// Tonight's spot job, labels as the queue held them (infra ci-pytest, 129221).
var fleetSpotTask = &model.Task{Labels: map[string]string{
	"org-id": "2", "platform": "linux", "repo": "Peregrine-Technology-Systems/peregrine-infrastructure", "tier": "spot",
}}

var compileTask = &model.Task{Labels: map[string]string{
	"agent": "pts-build", "org-id": "2", "repo": "Peregrine-Technology-Systems/woodpecker",
}}

func TestBuildVMTakesNoFleetSpotWork(t *testing.T) {
	matched, _ := createFilterFunc(buildVMFilter("pts-build"))(fleetSpotTask)
	assert.False(t, matched, "a spot task must never be scheduled to the build VM")

	matched, _ = createFilterFunc(buildVMFilter("pts-build"))(compileTask)
	assert.True(t, matched, "the build VM still takes its own compile")
}

// The failure this guards against: with tier defaulted to spot, the build VM
// matched the fleet job (pts-build 654 and 655).
func TestBuildVMWithTheSpotDefaultTookFleetWork(t *testing.T) {
	matched, _ := createFilterFunc(buildVMFilter("spot"))(fleetSpotTask)
	assert.True(t, matched)
}
