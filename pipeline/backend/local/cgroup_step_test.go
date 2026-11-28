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
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #423: each step of a contained workflow runs in its own cgroup,
// wf-<task>/step-<uuid>, so DestroyStep can kill what that step started
// without touching the rest of the workflow. Fake tree; the kernel behavior is
// pinned by cgroup_step_linux_test.go.

func fakeWorkflow(t *testing.T) (*cgroupFS, *workflowCgroup) {
	t.Helper()
	fs, root := fakeCgroupTree(t)
	wc, err := fs.create(root, "task-1")
	require.NoError(t, err)
	t.Cleanup(wc.close)
	return fs, wc
}

func TestStepCgroupCreate(t *testing.T) {
	t.Run("makes step-<uuid> inside the workflow cgroup and opens it", func(t *testing.T) {
		fs, wc := fakeWorkflow(t)

		sc, err := fs.createStep(wc, "s1")

		require.NoError(t, err)
		assert.Equal(t, filepath.Join(wc.dir, "step-s1"), sc.dir)
		assert.DirExists(t, sc.dir)
		assert.GreaterOrEqual(t, sc.fd, 0)
		sc.close()
	})

	for name, uuid := range map[string]string{"climbs out": "../../escape", "empty": "", "nested": "a/b"} {
		t.Run("refuses a step uuid that "+name, func(t *testing.T) {
			fs, wc := fakeWorkflow(t)

			_, err := fs.createStep(wc, uuid)

			require.ErrorIs(t, err, ErrCannotContain)
			entries, _ := os.ReadDir(wc.dir)
			assert.Empty(t, entries, "nothing is made")
			assert.NoDirExists(t, filepath.Join(filepath.Dir(wc.dir), "escape"))
		})
	}

	t.Run("a cgroup it cannot make refuses the step", func(t *testing.T) {
		fs, wc := fakeWorkflow(t)
		require.NoError(t, os.RemoveAll(wc.dir))

		_, err := fs.createStep(wc, "s1")

		require.ErrorIs(t, err, ErrCannotContain)
	})

	t.Run("a cgroup it cannot open is removed and refuses the step", func(t *testing.T) {
		fs, wc := fakeWorkflow(t)
		fs.openDir = func(string) (int, error) { return -1, errors.New("EPERM") }

		_, err := fs.createStep(wc, "s1")

		require.ErrorIs(t, err, ErrCannotContain)
		assert.ErrorContains(t, err, "EPERM")
		assert.NoDirExists(t, filepath.Join(wc.dir, "step-s1"))
	})
}

func TestStepCgroupDestroy(t *testing.T) {
	t.Run("kills the step only, waits for empty, removes, closes", func(t *testing.T) {
		fs, wc := fakeWorkflow(t)
		sc, err := fs.createStep(wc, "s1")
		require.NoError(t, err)
		setPopulated(t, sc.dir, true)
		var killed []string
		fs.kill = func(dir string) error { killed = append(killed, dir); setPopulated(t, dir, false); return nil }

		require.NoError(t, fs.destroyStep(wc, sc))

		assert.Equal(t, []string{sc.dir}, killed, "never the workflow cgroup")
		assert.NoDirExists(t, sc.dir)
		assert.DirExists(t, wc.dir)
		assert.Equal(t, -1, sc.fd)
	})

	t.Run("a step still populated at the bound is an error and is left as evidence", func(t *testing.T) {
		fs, wc := fakeWorkflow(t)
		dir := filepath.Join(wc.dir, "step-s1")
		fakeWorkflowCgroup(t, dir, true)
		fs.kill = func(string) error { return nil }

		err := fs.destroyStep(wc, &workflowCgroup{dir: dir, fd: -1})

		assert.ErrorContains(t, err, "still populated after 200ms")
		assert.ErrorContains(t, err, "4242 4343")
		assert.DirExists(t, dir)
	})

	t.Run("a failed kill write is returned and the cgroup kept", func(t *testing.T) {
		fs, wc := fakeWorkflow(t)
		dir := filepath.Join(wc.dir, "step-s1")
		fakeWorkflowCgroup(t, dir, true)
		fs.kill = func(string) error { return errors.New("EACCES") }

		err := fs.destroyStep(wc, &workflowCgroup{dir: dir, fd: -1})

		assert.ErrorContains(t, err, "EACCES")
		assert.DirExists(t, dir)
	})

	t.Run("a failed rmdir is returned", func(t *testing.T) {
		fs, wc := fakeWorkflow(t)
		dir := filepath.Join(wc.dir, "step-s1")
		fakeWorkflowCgroup(t, dir, false)
		fs.kill = func(string) error { return nil }
		fs.rmdir = func(string) error { return errors.New("EBUSY") }

		assert.ErrorContains(t, fs.destroyStep(wc, &workflowCgroup{dir: dir, fd: -1}), "EBUSY")
	})

	// The kill is guarded: only a step-* directly under this workflow's cgroup.
	for name, dir := range map[string]func(wf string) string{
		"the workflow cgroup":    func(wf string) string { return wf },
		"a sibling workflow":     func(wf string) string { return filepath.Join(filepath.Dir(wf), "wf-other") },
		"another workflow step":  func(wf string) string { return filepath.Join(filepath.Dir(wf), "wf-other", "step-s1") },
		"not a step-* name":      func(wf string) string { return filepath.Join(wf, "sub") },
		"nested below a step":    func(wf string) string { return filepath.Join(wf, "step-a", "step-b") },
		"a bare step- prefix":    func(wf string) string { return filepath.Join(wf, "step-") },
		"climbs out with dotdot": func(wf string) string { return filepath.Join(wf, "step-a") + "/../../wf-other" },
	} {
		t.Run("refuses to kill "+name, func(t *testing.T) {
			fs, wc := fakeWorkflow(t)
			fs.kill = func(string) error { t.Fatal("kill must not be called"); return nil }

			err := fs.destroyStep(wc, &workflowCgroup{dir: dir(wc.dir), fd: -1})

			assert.ErrorContains(t, err, "refusing to kill")
		})
	}
}
