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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"go.woodpecker-ci.org/woodpecker/v3/pipeline/backend/types"
)

// #412 against the real kernel. These tests need cgroup v2 and a cgroup this
// process may create children in, which on the CI fleet is the delegated
// workflow cgroup the test itself runs in. Without one they FAIL: a skip
// would report an uncontained fleet as fine.
//
// Every test works inside a scratch subtree below the test's own cgroup, so
// no kill can reach a sibling workflow or the agent.

func requireCgroupScratch(t *testing.T) (scratch string) {
	t.Helper()
	// The real delegation mark, on the parent of the cgroup this test runs
	// in. On the fleet the test is a CI job, so its own leaf is the job's
	// wf-<uuid>; the binary accepts only the agent leaf, so the helper widens
	// the accepted leaves for itself and still reads the real xattr (#412).
	// An undelegated host fails here rather than have the test write into a
	// subtree the service manager owns.
	probe := newCgroupFS()
	probe.checkDelegated = func(root, leaf string) error {
		if !strings.HasPrefix(leaf, wfCgroupPrefix) && leaf != "agent" {
			return fmt.Errorf("test runs in %q, neither the agent leaf nor a workflow", leaf)
		}
		return checkDelegatedSubtree(root, "agent")
	}
	_, own, err := probe.resolve()
	require.NoError(t, err, "workflow-cgroup tests need cgroup v2 and a delegated subtree (#412)")
	scratch = filepath.Join(own, fmt.Sprintf("itest-%d-%d", os.Getpid(), time.Now().UnixNano()))
	require.NoError(t, os.Mkdir(scratch, 0o755), "workflow-cgroup tests need a cgroup they can create children in (#412)")
	t.Cleanup(func() { killTree(t, scratch) })
	return scratch
}

// killTree empties and removes a scratch subtree, children first.
func killTree(t *testing.T, dir string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() {
			killTree(t, filepath.Join(dir, e.Name()))
		}
	}
	_ = writeCgroupKill(dir)
	for i := 0; i < 200; i++ {
		if p, err := readPopulated(dir); err != nil || !p {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
		t.Logf("scratch cgroup %s left behind: %v", dir, err)
	}
}

// realBackend is a contained backend whose delegated root is root: the
// process cgroup file it reads names a leaf under root.
func realBackend(t *testing.T, root string) *local {
	t.Helper()
	fs := newCgroupFS()
	fs.checkDelegated = func(string, string) error { return nil }
	fs.selfFile = filepath.Join(t.TempDir(), "cgroup")
	rel := strings.TrimPrefix(root, fs.mount)
	require.NoError(t, os.WriteFile(fs.selfFile, []byte("0::"+rel+"/agent\n"), 0o644))
	return &local{tempDir: t.TempDir(), contain: true, cgroups: fs}
}

func runCommands(t *testing.T, b *local, taskUUID string, commands ...string) string {
	t.Helper()
	step := &types.Step{UUID: "s-" + taskUUID, Name: "s", Type: types.StepTypeCommands, Image: "sh", Commands: commands}
	require.NoError(t, b.StartStep(context.Background(), step, taskUUID))
	out, err := b.TailStep(context.Background(), step, taskUUID)
	require.NoError(t, err)
	data, _ := io.ReadAll(out)
	st, err := b.WaitStep(context.Background(), step, taskUUID)
	require.NoError(t, err)
	require.Equal(t, 0, st.ExitCode, string(data))
	return string(data)
}

// pidsWithToken lists live, non-zombie processes whose argv carries token.
func pidsWithToken(token string) []string {
	var pids []string
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		cmdline, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil || !strings.Contains(string(cmdline), token) {
			continue
		}
		if stat, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat")); err == nil && isZombie(string(stat)) {
			continue
		}
		pids = append(pids, e.Name())
	}
	return pids
}

func isZombie(stat string) bool {
	i := strings.LastIndex(stat, ")")
	return i >= 0 && len(stat) > i+2 && stat[i+2] == 'Z'
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestContainedStepIsBornInsideItsWorkflowCgroup(t *testing.T) {
	b := realBackend(t, requireCgroupScratch(t))
	require.NoError(t, b.SetupWorkflow(context.Background(), nil, "born"))

	out := runCommands(t, b, "born", "cat /proc/self/cgroup")

	assert.Contains(t, out, "/wf-born\n", "the step must run in its workflow cgroup, not the agent's")
	require.NoError(t, b.DestroyWorkflow(context.Background(), nil, "born"))
}

// The escape test: three ways a job leaves its step's process tree. Each
// survives today's DestroyWorkflow; none may survive a contained one.
func TestDestroyWorkflowKillsDetachedChildren(t *testing.T) {
	b := realBackend(t, requireCgroupScratch(t))
	token := fmt.Sprintf("wp412-%d", time.Now().UnixNano())
	require.NoError(t, b.SetupWorkflow(context.Background(), nil, "escape"))

	runCommands(t, b, "escape",
		"setsid sh -c 'sleep 300; true' "+token+"-setsid >/dev/null 2>&1 < /dev/null &",
		"( ( sh -c 'sleep 300; true' "+token+"-doublefork >/dev/null 2>&1 < /dev/null & ) & )",
		"nohup sh -c 'sleep 300; true' "+token+"-nohup >/dev/null 2>&1 < /dev/null &",
	)
	// Baseline before verdict: all three escaped and outlived their step.
	waitFor(t, "the three escaped children", func() bool { return len(pidsWithToken(token)) == 3 })

	require.NoError(t, b.DestroyWorkflow(context.Background(), nil, "escape"))

	assert.Empty(t, pidsWithToken(token), "a detached child outlived its workflow")
}

// A job may make cgroups of its own inside its workflow cgroup (this very
// test suite does, on the fleet). Teardown must still kill and remove all.
func TestDestroyWorkflowRemovesCgroupsTheJobMade(t *testing.T) {
	scratch := requireCgroupScratch(t)
	b := realBackend(t, scratch)
	token := fmt.Sprintf("wp412-nested-%d", time.Now().UnixNano())
	require.NoError(t, b.SetupWorkflow(context.Background(), nil, "nested"))

	runCommands(t, b, "nested",
		"own=/sys/fs/cgroup$(sed -n 's/^0:://p' /proc/self/cgroup)",
		"mkdir \"$own/sub\"",
		"sh -c 'echo $$ > \"$1/sub/cgroup.procs\"; sleep 300; true' "+token+" \"$own\" >/dev/null 2>&1 < /dev/null &",
	)
	waitFor(t, "the child in the job's own cgroup", func() bool { return len(pidsWithToken(token)) == 1 })

	require.NoError(t, b.DestroyWorkflow(context.Background(), nil, "nested"))

	assert.Empty(t, pidsWithToken(token))
	assert.NoDirExists(t, filepath.Join(scratch, "wf-nested"))
}

func TestDestroyWorkflowNeverKillsOutsideTheWorkflow(t *testing.T) {
	scratch := requireCgroupScratch(t)
	b := realBackend(t, scratch)
	sibling := filepath.Join(scratch, "sibling")
	require.NoError(t, os.Mkdir(sibling, 0o755))
	fd, err := openCgroupDir(sibling)
	require.NoError(t, err)
	defer closeCgroupDir(fd)
	neighbor := exec.Command("sleep", "300")
	neighbor.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: fd}
	require.NoError(t, neighbor.Start())
	t.Cleanup(func() { _ = neighbor.Process.Kill(); _ = neighbor.Wait() })
	require.NoError(t, b.SetupWorkflow(context.Background(), nil, "fence"))
	runCommands(t, b, "fence", "true")

	require.NoError(t, b.DestroyWorkflow(context.Background(), nil, "fence"))

	assert.NoError(t, neighbor.Process.Signal(syscall.Signal(0)), "a process in a sibling cgroup was killed")
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", neighbor.Process.Pid))
	require.NoError(t, err)
	assert.False(t, isZombie(string(stat)), "a process in a sibling cgroup was killed")
	assert.NoError(t, syscall.Kill(os.Getpid(), 0), "the agent stand-in must survive")
}

// A cgroup the kill does not clear within the bound is an error, not a clean
// teardown, and stays as evidence. The kill is injected as a no-op: the
// kernel kills even frozen tasks, so a real cgroup.kill cannot be made to fail.
func TestDestroyWorkflowReportsAnUnkillableCgroup(t *testing.T) {
	scratch := requireCgroupScratch(t)
	b := realBackend(t, scratch)
	b.cgroups.kill = func(string) error { return nil }
	b.cgroups.killBound = 300 * time.Millisecond
	require.NoError(t, b.SetupWorkflow(context.Background(), nil, "stuck"))
	runCommands(t, b, "stuck", "setsid sleep 300 >/dev/null 2>&1 < /dev/null &")

	err := b.DestroyWorkflow(context.Background(), nil, "stuck")

	assert.ErrorContains(t, err, "still populated after 300ms")
	assert.DirExists(t, filepath.Join(scratch, "wf-stuck"))
}

func TestCheckDelegatedSubtree(t *testing.T) {
	scratch := requireCgroupScratch(t)

	err := checkDelegatedSubtree(scratch, "agent")
	assert.ErrorContains(t, err, "no delegate xattr on "+scratch)

	require.NoError(t, unix.Setxattr(scratch, "trusted.delegate", []byte("1"), 0))
	assert.NoError(t, checkDelegatedSubtree(scratch, "agent"))
	assert.ErrorContains(t, checkDelegatedSubtree(scratch, "wf-abc"), "is not the agent leaf", "a process in a workflow cgroup is not delegated")
	assert.ErrorContains(t, checkDelegatedSubtree(scratch, "init.scope"), "is not the agent leaf")
}
