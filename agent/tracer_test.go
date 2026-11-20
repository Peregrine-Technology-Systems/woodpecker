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

package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	backend "go.woodpecker-ci.org/woodpecker/v3/pipeline/backend/types"
	"go.woodpecker-ci.org/woodpecker/v3/pipeline/state"
	"go.woodpecker-ci.org/woodpecker/v3/rpc"
	rpc_mocks "go.woodpecker-ci.org/woodpecker/v3/rpc/mocks"
)

func traceState(exited bool) *state.State {
	s := &state.State{}
	s.Pipeline.Step = &backend.Step{UUID: "step-1", Image: "alpine"}
	s.Process.Exited = exited
	return s
}

func runTrace(t *testing.T, updateErr error, exited bool) (abandoned int, st *state.State) {
	t.Helper()
	peer := rpc_mocks.NewMockPeer(t)
	peer.On("Update", mock.Anything, "117374", mock.Anything).Return(updateErr)
	r := &Runner{client: peer, hostname: "ci-spot-us-eas-sfxz"}
	var uploads sync.WaitGroup
	trace := r.createTracer(context.Background(), &uploads, log.Logger,
		&rpc.Workflow{ID: "117374"}, func() { abandoned++ })
	st = traceState(exited)
	assert.NoError(t, trace(st))
	uploads.Wait()
	return abandoned, st
}

// #394: a step update the server rejected as terminal means the server has
// abandoned this workflow. The tracer must hand that to the runner to stop the
// workflow, instead of the agent carrying on with work nobody will accept.
func TestTracer_TerminalRejectionAbandonsWorkflow(t *testing.T) {
	err := fmt.Errorf("server: %w: %w: state=killed", rpc.ErrStepTerminal, rpc.ErrStepAbandoned)
	abandoned, _ := runTrace(t, err, true)
	assert.Equal(t, 1, abandoned)
}

// Any other update error is not a verdict on the workflow and must not stop it.
func TestTracer_OtherUpdateErrorDoesNotAbandon(t *testing.T) {
	abandoned, _ := runTrace(t, errors.New("ack timeout"), true)
	assert.Equal(t, 0, abandoned)
}

func TestTracer_RunningStepGetsMachineEnv(t *testing.T) {
	abandoned, st := runTrace(t, nil, false)
	assert.Equal(t, 0, abandoned)
	assert.Equal(t, "ci-spot-us-eas-sfxz", st.Pipeline.Step.Environment["CI_MACHINE"])
	assert.NotEmpty(t, st.Pipeline.Step.Environment["CI_SYSTEM_PLATFORM"])
}

// A terminal rejection of a result the server already recorded is dropped, and
// the workflow carries on.
func TestTracer_RecordedResultRejectionDoesNotAbandon(t *testing.T) {
	err := fmt.Errorf("server: %w: state=success", rpc.ErrStepTerminal)
	abandoned, _ := runTrace(t, err, true)
	assert.Equal(t, 0, abandoned)
}
