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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #412: the logic of workflow containment, against a fake cgroup tree in a
// temp dir. The real kernel behavior is pinned by cgroup_linux_test.go.

// fakeCgroupTree lays out a cgroup v2 mount with the process in
// <mount>/unit/agent and returns a cgroupFS pointed at it.
func fakeCgroupTree(t *testing.T) (*cgroupFS, string) {
	t.Helper()
	mount := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(mount, "cgroup.controllers"), nil, 0o644))
	own := filepath.Join(mount, "unit", "agent")
	require.NoError(t, os.MkdirAll(own, 0o755))
	self := filepath.Join(t.TempDir(), "cgroup")
	require.NoError(t, os.WriteFile(self, []byte("0::/unit/agent\n"), 0o644))
	fs := newCgroupFS()
	fs.mount = mount
	fs.selfFile = self
	fs.checkDelegated = func(string, string) error { return nil }
	fs.rmdir = os.RemoveAll // a fake cgroup holds regular files; a real rmdir would not
	fs.killBound = 200 * time.Millisecond
	fs.pollEvery = 5 * time.Millisecond
	return fs, filepath.Join(mount, "unit")
}

// fakeWorkflowCgroup makes dir look like a cgroup with the given populated state.
func fakeWorkflowCgroup(t *testing.T, dir string, populated bool) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	setPopulated(t, dir, populated)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte("4242\n4343\n"), 0o644))
}

func setPopulated(t *testing.T, dir string, populated bool) {
	t.Helper()
	v := "0"
	if populated {
		v = "1"
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cgroup.events"), []byte("populated "+v+"\nfrozen 0\n"), 0o644))
}

func TestParseCgroupV2Path(t *testing.T) {
	cases := []struct {
		name, in, want string
		wantErr        bool
	}{
		{name: "unified", in: "0::/system.slice/woodpecker-agent.service/agent\n", want: "/system.slice/woodpecker-agent.service/agent"},
		{name: "hybrid keeps only the v2 line", in: "12:cpu,cpuacct:/x\n0::/unit/agent\n1:name=systemd:/y\n", want: "/unit/agent"},
		{name: "no trailing newline", in: "0::/a", want: "/a"},
		{name: "v1 only", in: "12:cpu:/x\n", wantErr: true},
		{name: "empty", in: "", wantErr: true},
		{name: "v2 line with empty path", in: "0::\n", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCgroupV2Path([]byte(tc.in))
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestCgroupResolve(t *testing.T) {
	t.Run("returns the parent of the process's cgroup as the root", func(t *testing.T) {
		fs, root := fakeCgroupTree(t)
		var gotRoot, gotLeaf string
		fs.checkDelegated = func(r, l string) error { gotRoot, gotLeaf = r, l; return nil }

		r, own, err := fs.resolve()

		require.NoError(t, err)
		assert.Equal(t, root, r)
		assert.Equal(t, filepath.Join(root, "agent"), own)
		assert.Equal(t, root, gotRoot, "the delegation check must look at the root it hands out")
		assert.Equal(t, "agent", gotLeaf)
	})

	t.Run("refuses when cgroup v2 is not mounted", func(t *testing.T) {
		fs, _ := fakeCgroupTree(t)
		require.NoError(t, os.Remove(filepath.Join(fs.mount, "cgroup.controllers")))

		_, _, err := fs.resolve()

		require.ErrorIs(t, err, ErrCannotContain)
		assert.ErrorContains(t, err, "cgroup v2 not mounted at "+fs.mount)
	})

	t.Run("refuses when the process cgroup cannot be read", func(t *testing.T) {
		fs, _ := fakeCgroupTree(t)
		fs.selfFile = filepath.Join(t.TempDir(), "missing")

		_, _, err := fs.resolve()

		require.ErrorIs(t, err, ErrCannotContain)
		assert.ErrorContains(t, err, "read "+fs.selfFile)
	})

	t.Run("refuses when the process cgroup has no v2 entry", func(t *testing.T) {
		fs, _ := fakeCgroupTree(t)
		require.NoError(t, os.WriteFile(fs.selfFile, []byte("12:cpu:/x\n"), 0o644))

		_, _, err := fs.resolve()

		require.ErrorIs(t, err, ErrCannotContain)
		assert.ErrorContains(t, err, "no cgroup v2 entry")
	})

	t.Run("refuses when the process sits in the mount root", func(t *testing.T) {
		fs, _ := fakeCgroupTree(t)
		require.NoError(t, os.WriteFile(fs.selfFile, []byte("0::/\n"), 0o644))

		_, _, err := fs.resolve()

		require.ErrorIs(t, err, ErrCannotContain)
		assert.ErrorContains(t, err, "has no parent")
	})

	t.Run("refuses, naming the cause, when the subtree is not delegated", func(t *testing.T) {
		fs, root := fakeCgroupTree(t)
		fs.checkDelegated = func(string, string) error { return errors.New("no delegate xattr") }

		_, _, err := fs.resolve()

		require.ErrorIs(t, err, ErrCannotContain)
		assert.ErrorContains(t, err, "subtree "+root+" is not delegated: no delegate xattr")
	})
}

func TestCgroupCreate(t *testing.T) {
	t.Run("creates wf-<uuid> under the root and holds a directory fd", func(t *testing.T) {
		fs, root := fakeCgroupTree(t)

		wc, err := fs.create(root, "task-1")

		require.NoError(t, err)
		t.Cleanup(func() { wc.close() })
		assert.Equal(t, filepath.Join(root, "wf-task-1"), wc.dir)
		assert.DirExists(t, wc.dir)
		assert.GreaterOrEqual(t, wc.fd, 0)
	})

	t.Run("refuses, naming the path, when the root is not writable", func(t *testing.T) {
		fs, _ := fakeCgroupTree(t)
		root := filepath.Join(t.TempDir(), "absent")

		_, err := fs.create(root, "task-1")

		require.ErrorIs(t, err, ErrCannotContain)
		assert.ErrorContains(t, err, "delegated subtree "+root+" not writable")
	})

	t.Run("refuses and leaves nothing behind when the directory cannot be opened", func(t *testing.T) {
		fs, root := fakeCgroupTree(t)
		fs.openDir = func(string) (int, error) { return -1, errors.New("boom") }

		_, err := fs.create(root, "task-1")

		require.ErrorIs(t, err, ErrCannotContain)
		assert.ErrorContains(t, err, "open "+filepath.Join(root, "wf-task-1")+": boom")
		assert.NoDirExists(t, filepath.Join(root, "wf-task-1"))
	})

	t.Run("close is safe twice and on an unopened cgroup", func(t *testing.T) {
		(&workflowCgroup{fd: -1}).close()
		fs, root := fakeCgroupTree(t)
		wc, err := fs.create(root, "task-1")
		require.NoError(t, err)
		wc.close()
		wc.close()
		assert.Equal(t, -1, wc.fd)
	})
}

func TestCgroupDestroy(t *testing.T) {
	t.Run("kills, waits for empty, removes, closes", func(t *testing.T) {
		fs, root := fakeCgroupTree(t)
		wc, err := fs.create(root, "task-1")
		require.NoError(t, err)
		setPopulated(t, wc.dir, true)
		var killed string
		fs.kill = func(dir string) error { killed = dir; setPopulated(t, dir, false); return nil }

		require.NoError(t, fs.destroy(root, wc))

		assert.Equal(t, wc.dir, killed)
		assert.NoDirExists(t, wc.dir)
		assert.Equal(t, -1, wc.fd)
	})

	t.Run("an empty cgroup is still killed before removal", func(t *testing.T) {
		fs, root := fakeCgroupTree(t)
		fakeWorkflowCgroup(t, filepath.Join(root, "wf-task-1"), false)
		calls := 0
		fs.kill = func(string) error { calls++; return nil }

		require.NoError(t, fs.destroy(root, &workflowCgroup{dir: filepath.Join(root, "wf-task-1"), fd: -1}))

		assert.Equal(t, 1, calls)
	})

	t.Run("a cgroup still populated at the bound is an error and is left as evidence", func(t *testing.T) {
		fs, root := fakeCgroupTree(t)
		dir := filepath.Join(root, "wf-task-1")
		fakeWorkflowCgroup(t, dir, true)
		fs.kill = func(string) error { return nil } // the kill does not take

		err := fs.destroy(root, &workflowCgroup{dir: dir, fd: -1})

		require.Error(t, err)
		assert.ErrorContains(t, err, "still populated after 200ms")
		assert.ErrorContains(t, err, "4242 4343")
		assert.DirExists(t, dir)
	})

	t.Run("a failed kill write is returned and the cgroup kept", func(t *testing.T) {
		fs, root := fakeCgroupTree(t)
		dir := filepath.Join(root, "wf-task-1")
		fakeWorkflowCgroup(t, dir, true)
		fs.kill = func(string) error { return errors.New("EACCES") }

		err := fs.destroy(root, &workflowCgroup{dir: dir, fd: -1})

		assert.ErrorContains(t, err, "kill workflow cgroup "+dir+": EACCES")
		assert.DirExists(t, dir)
	})

	t.Run("an unreadable cgroup.events is an error, not a clean teardown", func(t *testing.T) {
		fs, root := fakeCgroupTree(t)
		dir := filepath.Join(root, "wf-task-1")
		require.NoError(t, os.MkdirAll(dir, 0o755))
		fs.kill = func(string) error { return nil }

		err := fs.destroy(root, &workflowCgroup{dir: dir, fd: -1})

		assert.ErrorContains(t, err, "cgroup.events")
		assert.DirExists(t, dir)
	})

	t.Run("a failed rmdir is returned", func(t *testing.T) {
		fs, root := fakeCgroupTree(t)
		dir := filepath.Join(root, "wf-task-1")
		fakeWorkflowCgroup(t, dir, false)
		fs.kill = func(string) error { return nil }
		fs.rmdir = func(string) error { return errors.New("EBUSY") }

		err := fs.destroy(root, &workflowCgroup{dir: dir, fd: -1})

		assert.ErrorContains(t, err, "remove workflow cgroup "+dir+": EBUSY")
	})

	// The kill write is the one act that can hit another repo's job, so it
	// is guarded: only a wf-* directory directly under the root.
	for name, dir := range map[string]func(root string) string{
		"outside the root":    func(string) string { return "/sys/fs/cgroup/wf-x" },
		"nested below a wf":   func(root string) string { return filepath.Join(root, "wf-a", "wf-b") },
		"not a wf-* name":     func(root string) string { return filepath.Join(root, "agent") },
		"the root itself":     func(root string) string { return root },
		"a bare wf- prefix":   func(root string) string { return filepath.Join(root, "wf-") },
		"climbs out with dot": func(root string) string { return filepath.Join(root, "wf-a") + "/../agent" },
	} {
		t.Run("refuses to kill "+name, func(t *testing.T) {
			fs, root := fakeCgroupTree(t)
			fs.kill = func(string) error { t.Fatal("kill must not be called"); return nil }

			err := fs.destroy(root, &workflowCgroup{dir: dir(root), fd: -1})

			assert.ErrorContains(t, err, "refusing to kill")
		})
	}
}

func TestRemoveTreeRemovesChildCgroupsDeepestFirst(t *testing.T) {
	fs, root := fakeCgroupTree(t)
	dir := filepath.Join(root, "wf-task-1")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub", "subsub"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub2"), 0o755))
	var removed []string
	fs.rmdir = func(d string) error { removed = append(removed, d); return os.Remove(d) }

	require.NoError(t, fs.removeTree(dir))

	assert.Equal(t, []string{
		filepath.Join(dir, "sub", "subsub"), filepath.Join(dir, "sub"), filepath.Join(dir, "sub2"), dir,
	}, removed)
	assert.NoDirExists(t, dir)
}

func TestRemoveTreeStopsAtTheFirstFailure(t *testing.T) {
	fs, root := fakeCgroupTree(t)
	dir := filepath.Join(root, "wf-task-1")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
	fs.rmdir = func(d string) error { return errors.New("EBUSY " + filepath.Base(d)) }

	assert.ErrorContains(t, fs.removeTree(dir), "EBUSY sub")
	assert.Error(t, fs.removeTree(filepath.Join(root, "absent")))
}

func TestReadPopulated(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		body    string
		want    bool
		wantErr bool
	}{
		{body: "populated 1\nfrozen 0\n", want: true},
		{body: "frozen 0\npopulated 0\n", want: false},
		{body: "frozen 0\n", wantErr: true},
		{body: "populated x\n", wantErr: true},
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "cgroup.events"), []byte(tc.body), 0o644))
		got, err := readPopulated(dir)
		if tc.wantErr {
			assert.Error(t, err, tc.body)
			continue
		}
		require.NoError(t, err, tc.body)
		assert.Equal(t, tc.want, got, tc.body)
	}
}

func TestCheckDelegation(t *testing.T) {
	xattrs := func(set map[string]string) func(string, string, []byte) (int, error) {
		return func(_, attr string, dest []byte) (int, error) {
			v, ok := set[attr]
			if !ok {
				return 0, errors.New("ENODATA")
			}
			return copy(dest, v), nil
		}
	}
	for name, tc := range map[string]struct {
		leaf  string
		set   map[string]string
		cause string
	}{
		"trusted mark, agent leaf":        {leaf: "agent", set: map[string]string{"trusted.delegate": "1"}},
		"user mark, agent leaf":           {leaf: "agent", set: map[string]string{"user.delegate": "1"}},
		"marked root but a workflow leaf": {leaf: "wf-abc", set: map[string]string{"trusted.delegate": "1"}, cause: "is not the agent leaf"},
		"no mark":                         {leaf: "agent", cause: "no delegate xattr on /u (trusted.delegate: ENODATA\nuser.delegate: ENODATA)"},
		"mark not 1":                      {leaf: "agent", set: map[string]string{"trusted.delegate": "0"}, cause: `trusted.delegate: value "0"`},
		"marked root but a foreign leaf":  {leaf: "init.scope", set: map[string]string{"trusted.delegate": "1"}, cause: "is not the agent leaf"},
	} {
		t.Run(name, func(t *testing.T) {
			err := checkDelegation("/u", tc.leaf, xattrs(tc.set))
			if tc.cause == "" {
				assert.NoError(t, err)
				return
			}
			assert.ErrorContains(t, err, tc.cause)
		})
	}
}

func TestDefaultKillWritesOne(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cgroup.kill"), nil, 0o644))

	require.NoError(t, writeCgroupKill(dir))

	got, err := os.ReadFile(filepath.Join(dir, "cgroup.kill"))
	require.NoError(t, err)
	assert.Equal(t, "1", string(got))
	assert.Error(t, writeCgroupKill(filepath.Join(dir, "absent")), "a kernel without cgroup.kill must fail loud")
}

func TestCgroupSweep(t *testing.T) {
	t.Run("removes empty and kills populated leftovers, leaves the rest alone", func(t *testing.T) {
		fs, root := fakeCgroupTree(t)
		fakeWorkflowCgroup(t, filepath.Join(root, "wf-empty"), false)
		fakeWorkflowCgroup(t, filepath.Join(root, "wf-busy"), true)
		require.NoError(t, os.WriteFile(filepath.Join(root, "wf-a-file"), nil, 0o644))
		var killed []string
		fs.kill = func(dir string) error { killed = append(killed, dir); setPopulated(t, dir, false); return nil }

		fs.sweep(root, filepath.Join(root, "agent"))

		assert.NoDirExists(t, filepath.Join(root, "wf-empty"))
		assert.NoDirExists(t, filepath.Join(root, "wf-busy"))
		assert.DirExists(t, filepath.Join(root, "agent"))
		assert.FileExists(t, filepath.Join(root, "wf-a-file"))
		// Both are killed: the populated state is read before the kill, and
		// a cgroup can gain a process in between, so emptiness is not trusted.
		assert.ElementsMatch(t, []string{filepath.Join(root, "wf-busy"), filepath.Join(root, "wf-empty")}, killed)
	})

	t.Run("does nothing from inside a workflow cgroup", func(t *testing.T) {
		fs, root := fakeCgroupTree(t)
		fakeWorkflowCgroup(t, filepath.Join(root, "wf-sibling"), true)
		fs.kill = func(string) error { t.Fatal("a job must never sweep its siblings"); return nil }

		fs.sweep(root, filepath.Join(root, "wf-me"))

		assert.DirExists(t, filepath.Join(root, "wf-sibling"))
	})

	t.Run("an unreadable root and a failed teardown are logged, not fatal", func(t *testing.T) {
		fs, root := fakeCgroupTree(t)
		fs.sweep(filepath.Join(t.TempDir(), "absent"), filepath.Join(root, "agent"))

		fakeWorkflowCgroup(t, filepath.Join(root, "wf-stuck"), true)
		fs.kill = func(string) error { return nil }
		fs.sweep(root, filepath.Join(root, "agent"))

		assert.DirExists(t, filepath.Join(root, "wf-stuck"), "a cgroup that would not die is kept as evidence")
	})
}
