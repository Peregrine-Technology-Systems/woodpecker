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

//go:build !linux

// These start real steps against a fake cgroup tree, which only works where a
// step is not born into the cgroup by fd. On Linux the same behavior is pinned
// against the kernel by cgroup_step_linux_test.go.

package local

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.woodpecker-ci.org/woodpecker/v3/pipeline/backend/types"
)

// #423: how the backend uses step cgroups, against a fake cgroup tree.

func startShellStep(t *testing.T, b *local, taskUUID, stepUUID string) *types.Step {
	t.Helper()
	step := &types.Step{UUID: stepUUID, Name: stepUUID, Type: types.StepTypeCommands, Image: "sh", Commands: []string{"true"}}
	require.NoError(t, b.StartStep(context.Background(), step, taskUUID))
	out, err := b.TailStep(context.Background(), step, taskUUID)
	require.NoError(t, err)
	_, _ = io.ReadAll(out)
	_, err = b.WaitStep(context.Background(), step, taskUUID)
	require.NoError(t, err)
	return step
}

func TestContainedStepGetsItsOwnCgroupAndDestroyStepKillsIt(t *testing.T) {
	b, fs, root := containedBackend(t)
	var killed []string
	fs.kill = func(dir string) error { killed = append(killed, dir); setPopulated(t, dir, false); return nil }
	require.NoError(t, b.SetupWorkflow(context.Background(), nil, "task-1"))
	wf := filepath.Join(root, "wf-task-1")

	step := startShellStep(t, b, "task-1", "s1")
	st, err := b.getStepState("task-1", "s1")
	require.NoError(t, err)
	require.NotNil(t, st.cgroup, "a contained step has its own cgroup")
	assert.Equal(t, filepath.Join(wf, "step-s1"), st.cgroup.dir)

	require.NoError(t, b.DestroyStep(context.Background(), step, "task-1"))

	assert.Equal(t, []string{filepath.Join(wf, "step-s1")}, killed, "DestroyStep kills the step's cgroup, not the workflow's")
	assert.NoDirExists(t, filepath.Join(wf, "step-s1"))
	assert.DirExists(t, wf)
	require.NoError(t, b.DestroyWorkflow(context.Background(), nil, "task-1"))
}

func TestDestroyStepReportsSurvivors(t *testing.T) {
	b, fs, root := containedBackend(t)
	require.NoError(t, b.SetupWorkflow(context.Background(), nil, "task-1"))
	step := startShellStep(t, b, "task-1", "s1")
	dir := filepath.Join(root, "wf-task-1", "step-s1")
	fakeWorkflowCgroup(t, dir, true)
	fs.kill = func(string) error { return nil } // the kill does not take

	err := b.DestroyStep(context.Background(), step, "task-1")

	assert.ErrorContains(t, err, "still populated")
	assert.DirExists(t, dir, "evidence is kept")
	_, err = b.getStepState("task-1", "s1")
	assert.ErrorIs(t, err, ErrStepStateNotFound, "the step is forgotten either way")
}

func TestAStepThatCannotBeContainedDoesNotStart(t *testing.T) {
	b, fs, _ := containedBackend(t)
	require.NoError(t, b.SetupWorkflow(context.Background(), nil, "task-1"))
	fs.openDir = func(dir string) (int, error) {
		if strings.Contains(dir, "step-") {
			return -1, errors.New("EPERM")
		}
		return openCgroupDir(dir)
	}

	for _, typ := range []types.StepType{types.StepTypeCommands, types.StepTypePlugin} {
		step := &types.Step{UUID: "s-" + string(typ), Type: typ, Image: "sh", Commands: []string{"true"}}

		err := b.StartStep(context.Background(), step, "task-1")

		require.ErrorIs(t, err, ErrCannotContain, string(typ))
		_, err = b.getStepState("task-1", step.UUID)
		assert.ErrorIs(t, err, ErrStepStateNotFound, "no process was started for %s", typ)
	}
	require.NoError(t, b.DestroyWorkflow(context.Background(), nil, "task-1"))
}

func TestDestroyWorkflowClosesStepCgroupsLeftOpen(t *testing.T) {
	b, _, _ := containedBackend(t)
	require.NoError(t, b.SetupWorkflow(context.Background(), nil, "task-1"))
	startShellStep(t, b, "task-1", "s1")
	st, err := b.getStepState("task-1", "s1")
	require.NoError(t, err)

	require.NoError(t, b.DestroyWorkflow(context.Background(), nil, "task-1"))

	assert.Equal(t, -1, st.cgroup.fd, "a step cgroup DestroyStep never saw is closed at teardown")
}

func TestUncontainedStepHasNoCgroup(t *testing.T) {
	b := &local{tempDir: t.TempDir()}
	require.NoError(t, b.SetupWorkflow(context.Background(), nil, "task-1"))
	step := startShellStep(t, b, "task-1", "s1")
	st, err := b.getStepState("task-1", "s1")
	require.NoError(t, err)
	assert.Nil(t, st.cgroup)
	require.NoError(t, b.DestroyStep(context.Background(), step, "task-1"))
	require.NoError(t, b.DestroyWorkflow(context.Background(), nil, "task-1"))
}
