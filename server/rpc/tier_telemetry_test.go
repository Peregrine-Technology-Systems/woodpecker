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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	corepipeline "go.woodpecker-ci.org/woodpecker/v3/pipeline"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	store_mocks "go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
)

var tierBypassPatterns = []string{"deploy", "promote", "version-bump"}

func TestShouldEmitTierBypass(t *testing.T) {
	cases := []struct {
		name     string
		tier     string
		workflow string
		event    model.WebhookEvent
		want     bool
	}{
		// spot + deploy-class workflow name → bypass
		{"spot + deploy workflow", "spot", "deploy", model.EventPush, true},
		{"spot + promote workflow", "spot", "promote-to-staging", model.EventPush, true},
		{"spot + version-bump workflow", "spot", "version-bump", model.EventPush, true},
		// spot + tag/deployment event (even with a non-deploy workflow name) → bypass
		{"spot + ci on tag event", "spot", "ci", model.EventTag, true},
		{"spot + ci on deployment event", "spot", "ci", model.EventDeploy, true},
		// spot + ordinary CI → NOT a bypass (the common, correct case)
		{"spot + ci on push", "spot", "ci", model.EventPush, false},
		// non-spot tiers never emit, even for deploy-class work (they're the
		// correct destination). Untiered ("") is the persistent local box and
		// must NOT be treated as spot — else every local deploy false-positives.
		{"ondemand + deploy", "ondemand", "deploy", model.EventTag, false},
		{"n2 + deploy", "n2", "deploy", model.EventPush, false},
		{"untiered (local box) + deploy", "", "deploy", model.EventTag, false},
		{"untiered + tag", "", "ci", model.EventTag, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, shouldEmitTierBypass(tc.tier, tc.workflow, tc.event, tierBypassPatterns))
		})
	}
}

func newTierBypassRPC(s *store_mocks.MockStore) *RPC {
	return &RPC{store: s, deployPatterns: tierBypassPatterns}
}

func TestEmitTierBypassIfSpotDeploy_GuardsSkipWithoutStoreReads(t *testing.T) {
	// nil agent, nil task, and nil CustomLabels must all short-circuit before any
	// store read. NewMockStore(t) with no .On() asserts no unexpected calls.
	spotTask := &model.Task{PipelineID: 1, Name: "deploy"}
	spotAgent := &model.Agent{ID: 1, CustomLabels: map[string]string{corepipeline.LabelFilterTier: tierSpot}}

	newTierBypassRPC(store_mocks.NewMockStore(t)).emitTierBypassIfSpotDeploy(nil, spotTask)
	newTierBypassRPC(store_mocks.NewMockStore(t)).emitTierBypassIfSpotDeploy(spotAgent, nil)
	newTierBypassRPC(store_mocks.NewMockStore(t)).emitTierBypassIfSpotDeploy(&model.Agent{ID: 1}, spotTask) // nil CustomLabels
}

func TestEmitTierBypassIfSpotDeploy_NonSpotAgentNoStoreRead(t *testing.T) {
	// An ondemand agent must not even read the store — the cheap tier pre-check
	// short-circuits the common case.
	s := store_mocks.NewMockStore(t)
	agent := &model.Agent{ID: 2, CustomLabels: map[string]string{corepipeline.LabelFilterTier: "ondemand"}}
	newTierBypassRPC(s).emitTierBypassIfSpotDeploy(agent, &model.Task{PipelineID: 1, Name: "deploy"})
}

func TestEmitTierBypassIfSpotDeploy_PipelineLoadErrorIsBestEffort(t *testing.T) {
	s := store_mocks.NewMockStore(t)
	s.On("GetPipeline", int64(7)).Return((*model.Pipeline)(nil), errors.New("boom"))
	agent := &model.Agent{ID: 3, CustomLabels: map[string]string{corepipeline.LabelFilterTier: tierSpot}}
	// GetRepo must NOT be called when the pipeline load fails.
	newTierBypassRPC(s).emitTierBypassIfSpotDeploy(agent, &model.Task{PipelineID: 7, Name: "deploy"})
}

func TestEmitTierBypassIfSpotDeploy_SpotNonDeployDoesNotReadRepo(t *testing.T) {
	// Spot agent pulling ordinary CI: pipeline is read to check the event, but it
	// is not deploy-class, so no repo read and no emit.
	s := store_mocks.NewMockStore(t)
	s.On("GetPipeline", int64(8)).Return(&model.Pipeline{Number: 8, RepoID: 99, Event: model.EventPush}, nil)
	agent := &model.Agent{ID: 4, CustomLabels: map[string]string{corepipeline.LabelFilterTier: tierSpot}}
	newTierBypassRPC(s).emitTierBypassIfSpotDeploy(agent, &model.Task{PipelineID: 8, Name: "ci"})
}

func TestEmitTierBypassIfSpotDeploy_SpotDeployEmits(t *testing.T) {
	// The bypass case: spot agent + deploy-class workflow. Both store reads happen
	// and EmitEvent is reached (a no-op without a configured plugin registry, so
	// this also proves the emit path is panic-safe in that configuration).
	s := store_mocks.NewMockStore(t)
	s.On("GetPipeline", int64(9)).Return(&model.Pipeline{Number: 9, RepoID: 42, Event: model.EventPush}, nil)
	s.On("GetRepo", int64(42)).Return(&model.Repo{ID: 42, FullName: "org/repo"}, nil)
	agent := &model.Agent{ID: 5, Name: "spot-vm-1", CustomLabels: map[string]string{corepipeline.LabelFilterTier: tierSpot}}
	newTierBypassRPC(s).emitTierBypassIfSpotDeploy(agent, &model.Task{PipelineID: 9, Name: "deploy"})
	s.AssertCalled(t, "GetRepo", int64(42))
}

func TestEmitTierBypassIfSpotDeploy_RepoLoadErrorIsBestEffort(t *testing.T) {
	s := store_mocks.NewMockStore(t)
	s.On("GetPipeline", int64(10)).Return(&model.Pipeline{Number: 10, RepoID: 50, Event: model.EventTag}, nil)
	s.On("GetRepo", int64(50)).Return((*model.Repo)(nil), errors.New("repo gone"))
	agent := &model.Agent{ID: 6, CustomLabels: map[string]string{corepipeline.LabelFilterTier: tierSpot}}
	// Tag event makes it deploy-class even though the workflow name is "ci".
	newTierBypassRPC(s).emitTierBypassIfSpotDeploy(agent, &model.Task{PipelineID: 10, Name: "ci"})
}
