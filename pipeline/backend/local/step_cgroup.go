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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Step containment (#423). Inside a contained workflow each step runs in its
// own cgroup, wf-<task>/step-<uuid>, and DestroyStep kills it, so what a
// cancelled or finished step started, setsid escapes included, does not run
// on beside the steps after it. The workflow teardown (#412) still kills the
// whole workflow cgroup, step cgroups included.

const stepCgroupPrefix = "step-"

// isStepCgroup guards the step kill: only a step-* directory directly under
// this workflow's cgroup.
func isStepCgroup(wfDir, dir string) bool {
	dir = filepath.Clean(dir)
	base := filepath.Base(dir)
	return filepath.Dir(dir) == filepath.Clean(wfDir) &&
		strings.HasPrefix(base, stepCgroupPrefix) && len(base) > len(stepCgroupPrefix)
}

// createStep makes the step's cgroup. A step that cannot be contained must
// not start.
func (c *cgroupFS) createStep(wf *workflowCgroup, stepUUID string) (*workflowCgroup, error) {
	dir := filepath.Join(wf.dir, stepCgroupPrefix+stepUUID)
	if stepUUID == "" || strings.ContainsRune(stepUUID, '/') || !isStepCgroup(wf.dir, dir) {
		return nil, fmt.Errorf("%w: step uuid %q does not name a cgroup in %s", ErrCannotContain, stepUUID, wf.dir)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		return nil, fmt.Errorf("%w: step cgroup %s: %w", ErrCannotContain, dir, err)
	}
	fd, err := c.openDir(dir)
	if err != nil {
		_ = c.rmdir(dir)
		return nil, fmt.Errorf("%w: open %s: %w", ErrCannotContain, dir, err)
	}
	return &workflowCgroup{dir: dir, fd: fd}, nil
}

// destroyStep kills every process in the step's cgroup, verifies it emptied,
// and removes it. One that does not empty is an error and is kept as evidence.
func (c *cgroupFS) destroyStep(wf, sc *workflowCgroup) error {
	defer sc.close()
	if !isStepCgroup(wf.dir, sc.dir) {
		return fmt.Errorf("local backend: refusing to kill %s: not a step cgroup under %s", sc.dir, wf.dir)
	}
	return c.killAndRemove(sc.dir, "step")
}

// containStep gives a contained step its own cgroup and has cmd born in it.
// An uncontained workflow returns nil.
func (e *local) containStep(cmd *exec.Cmd, state *workflowState, stepUUID string) (*workflowCgroup, error) {
	if state.cgroup == nil {
		return nil, nil
	}
	sc, err := e.cgroups.createStep(state.cgroup, stepUUID)
	if err != nil {
		return nil, err
	}
	bornInCgroup(cmd, sc)
	return sc, nil
}
