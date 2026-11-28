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

//go:build linux

package local

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.woodpecker-ci.org/woodpecker/v3/pipeline/backend/types"
)

// #423 against the real kernel: DestroyStep kills what that step started,
// setsid escapes included, and nothing else in the workflow.

func runStep(t *testing.T, b *local, taskUUID, stepUUID string, commands ...string) (*types.Step, string) {
	t.Helper()
	step := &types.Step{UUID: stepUUID, Name: stepUUID, Type: types.StepTypeCommands, Image: "sh", Commands: commands}
	require.NoError(t, b.StartStep(context.Background(), step, taskUUID))
	out, err := b.TailStep(context.Background(), step, taskUUID)
	require.NoError(t, err)
	data, _ := io.ReadAll(out)
	st, err := b.WaitStep(context.Background(), step, taskUUID)
	require.NoError(t, err)
	require.Equal(t, 0, st.ExitCode, string(data))
	return step, string(data)
}

func TestContainedStepIsBornInsideItsStepCgroup(t *testing.T) {
	b := realBackend(t, requireCgroupScratch(t))
	require.NoError(t, b.SetupWorkflow(context.Background(), nil, "born"))

	_, out := runStep(t, b, "born", "s1", "cat /proc/self/cgroup")

	assert.Contains(t, out, "/wf-born/step-s1\n", "the step must run in its own cgroup inside its workflow's")
	require.NoError(t, b.DestroyWorkflow(context.Background(), nil, "born"))
}

func TestDestroyStepKillsThatStepsEscapedChildrenOnly(t *testing.T) {
	b := realBackend(t, requireCgroupScratch(t))
	token := fmt.Sprintf("wp423-%d", time.Now().UnixNano())
	require.NoError(t, b.SetupWorkflow(context.Background(), nil, "steps"))
	escape := func(tag string) string {
		return "setsid sh -c 'sleep 300; true' " + token + "-" + tag + " >/dev/null 2>&1 < /dev/null &"
	}

	cancelled, _ := runStep(t, b, "steps", "cancelled", escape("cancelled"))
	sibling, _ := runStep(t, b, "steps", "sibling", escape("sibling"))
	// Baseline before verdict: both escaped and outlived their steps.
	waitFor(t, "both escaped children", func() bool { return len(pidsWithToken(token)) == 2 })

	require.NoError(t, b.DestroyStep(context.Background(), cancelled, "steps"))

	waitFor(t, "the cancelled step's child to die", func() bool { return len(pidsWithToken(token+"-cancelled")) == 0 })
	assert.Len(t, pidsWithToken(token+"-sibling"), 1, "a sibling step's process must survive")
	runStep(t, b, "steps", "next", "true") // the workflow carries on

	require.NoError(t, b.DestroyStep(context.Background(), sibling, "steps"))
	require.NoError(t, b.DestroyWorkflow(context.Background(), nil, "steps"))
	assert.Empty(t, pidsWithToken(token))
}
