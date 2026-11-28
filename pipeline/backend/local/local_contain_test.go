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

package local

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.woodpecker-ci.org/woodpecker/v3/pipeline/backend/types"
)

// #412: how the backend uses containment, against a fake cgroup tree.

func containedBackend(t *testing.T) (*local, *cgroupFS, string) {
	t.Helper()
	fs, root := fakeCgroupTree(t)
	fs.kill = func(dir string) error { setPopulated(t, dir, false); return nil }
	return &local{tempDir: t.TempDir(), contain: true, cgroups: fs}, fs, root
}

func TestNewAgentContainsWhereSupported(t *testing.T) {
	b, ok := NewAgent().(*local)
	require.True(t, ok)
	assert.Equal(t, containmentSupported, b.contain)
	assert.NotNil(t, b.cgroups)
	plain, _ := New().(*local)
	assert.False(t, plain.contain, "woodpecker-cli exec is not contained")
}

func TestSetupWorkflowRefusesWhatItCannotContain(t *testing.T) {
	for name, tc := range map[string]struct {
		breakIt func(t *testing.T, fs *cgroupFS)
		cause   string
	}{
		"cgroup v2 not mounted": {
			breakIt: func(t *testing.T, fs *cgroupFS) {
				require.NoError(t, os.Remove(filepath.Join(fs.mount, "cgroup.controllers")))
			},
			cause: "cgroup v2 not mounted",
		},
		"subtree not delegated": {
			breakIt: func(_ *testing.T, fs *cgroupFS) {
				fs.checkDelegated = func(string, string) error { return errors.New("no delegate xattr") }
			},
			cause: "is not delegated: no delegate xattr",
		},
		"subtree not writable": {
			breakIt: func(t *testing.T, fs *cgroupFS) {
				require.NoError(t, os.WriteFile(fs.selfFile, []byte("0::/absent/agent\n"), 0o644))
			},
			cause: "not writable",
		},
		"cgroup cannot be opened": {
			breakIt: func(_ *testing.T, fs *cgroupFS) {
				fs.openDir = func(string) (int, error) { return -1, errors.New("EPERM") }
			},
			cause: "EPERM",
		},
	} {
		t.Run(name, func(t *testing.T) {
			b, fs, _ := containedBackend(t)
			tc.breakIt(t, fs)

			err := b.SetupWorkflow(context.Background(), nil, "task-1")

			require.ErrorIs(t, err, ErrCannotContain)
			assert.ErrorContains(t, err, tc.cause)
			entries, _ := os.ReadDir(b.tempDir)
			assert.Empty(t, entries, "a refused workflow leaves no workspace")
			step := &types.Step{UUID: "s1", Type: types.StepTypeCommands, Image: "sh"}
			assert.ErrorIs(t, b.StartStep(context.Background(), step, "task-1"), ErrWorkflowStateNotFound, "no step may run")
		})
	}
}

func TestSetupWorkflowReleasesTheCgroupWhenTheWorkspaceFails(t *testing.T) {
	b, _, root := containedBackend(t)
	b.tempDir = filepath.Join(t.TempDir(), "absent")

	err := b.SetupWorkflow(context.Background(), nil, "task-1")

	require.Error(t, err)
	assert.NoDirExists(t, filepath.Join(root, "wf-task-1"))
}

func TestContainedWorkflowLifecycle(t *testing.T) {
	b, fs, root := containedBackend(t)
	var killed []string
	fs.kill = func(dir string) error { killed = append(killed, dir); setPopulated(t, dir, false); return nil }

	require.NoError(t, b.SetupWorkflow(context.Background(), nil, "task-1"))
	state, err := b.getWorkflowState("task-1")
	require.NoError(t, err)
	require.NotNil(t, state.cgroup)
	assert.Equal(t, filepath.Join(root, "wf-task-1"), state.cgroup.dir)

	require.NoError(t, b.DestroyWorkflow(context.Background(), nil, "task-1"))

	assert.Equal(t, []string{filepath.Join(root, "wf-task-1")}, killed)
	assert.NoDirExists(t, filepath.Join(root, "wf-task-1"))
	_, err = b.getWorkflowState("task-1")
	assert.ErrorIs(t, err, ErrWorkflowStateNotFound)
}

func TestDestroyWorkflowReportsSurvivors(t *testing.T) {
	b, fs, root := containedBackend(t)
	require.NoError(t, b.SetupWorkflow(context.Background(), nil, "task-1"))
	state, err := b.getWorkflowState("task-1")
	require.NoError(t, err)
	fakeWorkflowCgroup(t, state.cgroup.dir, true)
	fs.kill = func(string) error { return nil } // the kill does not take

	err = b.DestroyWorkflow(context.Background(), nil, "task-1")

	assert.ErrorContains(t, err, "still populated")
	assert.DirExists(t, filepath.Join(root, "wf-task-1"), "evidence is kept")
	assert.NoDirExists(t, state.baseDir, "the workspace is still removed")
}

func TestDestroyWorkflowJoinsWorkspaceAndCgroupErrors(t *testing.T) {
	b, fs, _ := containedBackend(t)
	require.NoError(t, b.SetupWorkflow(context.Background(), nil, "task-1"))
	state, err := b.getWorkflowState("task-1")
	require.NoError(t, err)
	fs.kill = func(string) error { return errors.New("EACCES") }
	state.baseDir = "\x00" // RemoveAll rejects a NUL in a path

	err = b.DestroyWorkflow(context.Background(), nil, "task-1")

	assert.ErrorContains(t, err, "EACCES")
	assert.ErrorContains(t, err, "invalid argument")
}

func TestLoadSweepsLeftoversWhenContained(t *testing.T) {
	b, _, root := containedBackend(t)
	fakeWorkflowCgroup(t, filepath.Join(root, "wf-old"), true)

	_, err := b.Load(context.Background())

	require.NoError(t, err)
	assert.NoDirExists(t, filepath.Join(root, "wf-old"))
}

func TestLoadDoesNotFailWhenContainmentIsUnavailable(t *testing.T) {
	b, fs, root := containedBackend(t)
	fakeWorkflowCgroup(t, filepath.Join(root, "wf-old"), true)
	fs.checkDelegated = func(string, string) error { return errors.New("no delegate xattr") }

	_, err := b.Load(context.Background())

	require.NoError(t, err, "SetupWorkflow refuses each workflow by name instead")
	assert.DirExists(t, filepath.Join(root, "wf-old"), "nothing is swept under an undelegated root")
}
