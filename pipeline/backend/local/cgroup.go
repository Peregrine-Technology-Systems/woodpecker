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
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

// Workflow containment (#412). Each workflow runs in its own cgroup v2,
// wf-<taskUUID>, a sibling of the agent's own cgroup under the subtree the
// service manager delegates to the agent. Every step is born inside it, and
// DestroyWorkflow kills the whole cgroup, so a child a job detached (setsid,
// a double fork, nohup &) cannot outlive its workflow and run into the next
// one. The agent refuses a workflow it cannot contain; it never falls back
// to running it uncontained.

// ErrCannotContain prefixes every reason the backend refuses a workflow.
var ErrCannotContain = errors.New("local backend: cannot contain workflow")

const wfCgroupPrefix = "wf-"

type cgroupFS struct {
	mount          string // the cgroup v2 mount
	selfFile       string // the process's own /proc/self/cgroup
	killBound      time.Duration
	pollEvery      time.Duration
	checkDelegated func(root, leaf string) error
	openDir        func(dir string) (int, error)
	kill           func(dir string) error
	rmdir          func(dir string) error
}

func newCgroupFS() *cgroupFS {
	return &cgroupFS{
		mount:          "/sys/fs/cgroup",
		selfFile:       "/proc/self/cgroup",
		killBound:      10 * time.Second,
		pollEvery:      50 * time.Millisecond,
		checkDelegated: checkDelegatedSubtree,
		openDir:        openCgroupDir,
		kill:           writeCgroupKill,
		rmdir:          os.Remove,
	}
}

type workflowCgroup struct {
	dir string
	fd  int // -1 once closed
}

func (wc *workflowCgroup) close() {
	if wc.fd >= 0 {
		closeCgroupDir(wc.fd)
		wc.fd = -1
	}
}

// parseCgroupV2Path returns the path of the unified (v2) hierarchy entry,
// the "0::" line of /proc/<pid>/cgroup.
func parseCgroupV2Path(data []byte) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		if p, ok := strings.CutPrefix(sc.Text(), "0::"); ok && p != "" {
			return p, nil
		}
	}
	return "", errors.New("no cgroup v2 entry")
}

// resolve returns the delegated root, the parent of the process's own
// cgroup, and the process's own cgroup. Every failure is ErrCannotContain
// with its cause named.
func (c *cgroupFS) resolve() (root, own string, err error) {
	if _, err := os.Stat(filepath.Join(c.mount, "cgroup.controllers")); err != nil {
		return "", "", fmt.Errorf("%w: cgroup v2 not mounted at %s", ErrCannotContain, c.mount)
	}
	data, err := os.ReadFile(c.selfFile)
	if err != nil {
		return "", "", fmt.Errorf("%w: read %s: %w", ErrCannotContain, c.selfFile, err)
	}
	rel, err := parseCgroupV2Path(data)
	if err != nil {
		return "", "", fmt.Errorf("%w: %s: %w", ErrCannotContain, c.selfFile, err)
	}
	own = filepath.Join(c.mount, rel)
	root = filepath.Dir(own)
	if own == filepath.Clean(c.mount) {
		return "", "", fmt.Errorf("%w: cgroup %s has no parent to hold workflows", ErrCannotContain, own)
	}
	if err := c.checkDelegated(root, filepath.Base(own)); err != nil {
		return "", "", fmt.Errorf("%w: subtree %s is not delegated: %w", ErrCannotContain, root, err)
	}
	return root, own, nil
}

func (c *cgroupFS) create(root, taskUUID string) (*workflowCgroup, error) {
	dir := filepath.Join(root, wfCgroupPrefix+taskUUID)
	if err := os.Mkdir(dir, 0o755); err != nil {
		return nil, fmt.Errorf("%w: delegated subtree %s not writable: %w", ErrCannotContain, root, err)
	}
	fd, err := c.openDir(dir)
	if err != nil {
		_ = c.rmdir(dir)
		return nil, fmt.Errorf("%w: open %s: %w", ErrCannotContain, dir, err)
	}
	return &workflowCgroup{dir: dir, fd: fd}, nil
}

// isWorkflowCgroup guards the kill write, the one act that can reach
// another repo's job: only a wf-* directory directly under root.
func isWorkflowCgroup(root, dir string) bool {
	dir = filepath.Clean(dir)
	base := filepath.Base(dir)
	return filepath.Dir(dir) == filepath.Clean(root) &&
		strings.HasPrefix(base, wfCgroupPrefix) && len(base) > len(wfCgroupPrefix)
}

// destroy kills every process in the workflow cgroup, verifies the cgroup
// emptied, and removes it. A cgroup that does not empty within the bound is
// an error and is left in place as evidence.
func (c *cgroupFS) destroy(root string, wc *workflowCgroup) error {
	defer wc.close()
	if !isWorkflowCgroup(root, wc.dir) {
		return fmt.Errorf("local backend: refusing to kill %s: not a workflow cgroup under %s", wc.dir, root)
	}
	if err := c.kill(wc.dir); err != nil {
		return fmt.Errorf("local backend: kill workflow cgroup %s: %w", wc.dir, err)
	}
	if err := c.waitEmpty(wc.dir); err != nil {
		return err
	}
	if err := c.removeTree(wc.dir); err != nil {
		return fmt.Errorf("local backend: remove workflow cgroup %s: %w", wc.dir, err)
	}
	return nil
}

// removeTree removes an emptied cgroup and any child cgroups a job made in
// it, deepest first: a cgroup with children cannot be removed.
func (c *cgroupFS) removeTree(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			if err := c.removeTree(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
	}
	return c.rmdir(dir)
}

func (c *cgroupFS) waitEmpty(dir string) error {
	deadline := time.Now().Add(c.killBound)
	for {
		populated, err := readPopulated(dir)
		if err != nil {
			return fmt.Errorf("local backend: workflow cgroup %s: %w", dir, err)
		}
		if !populated {
			return nil
		}
		if time.Now().After(deadline) {
			procs, _ := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
			return fmt.Errorf("local backend: workflow cgroup %s still populated after %s, pids: %s",
				dir, c.killBound, strings.Join(strings.Fields(string(procs)), " "))
		}
		time.Sleep(c.pollEvery)
	}
}

func readPopulated(dir string) (bool, error) {
	data, err := os.ReadFile(filepath.Join(dir, "cgroup.events"))
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "populated "); ok {
			switch v {
			case "0":
				return false, nil
			case "1":
				return true, nil
			}
			return false, fmt.Errorf("cgroup.events: bad populated value %q", v)
		}
	}
	return false, errors.New("cgroup.events: no populated line")
}

// checkDelegation asserts the service manager delegated root to the agent.
// Write access is not evidence: the agent is root and could create a cgroup
// under a slice it does not own. systemd marks a delegated cgroup with a
// delegate xattr, and the agent's own leaf is "agent" (DelegateSubgroup=agent).
// Exactly that shape passes: a process inside a workflow's cgroup must not
// count as delegated (INF ruling on #412). The xattr that matched is logged.
func checkDelegation(root, leaf string, getxattr func(path, attr string, dest []byte) (int, error)) error {
	if leaf != "agent" {
		return fmt.Errorf("process cgroup %q is not the agent leaf", filepath.Join(root, leaf))
	}
	var errs []error
	for _, attr := range []string{"trusted.delegate", "user.delegate"} {
		buf := make([]byte, 8)
		n, err := getxattr(root, attr, buf)
		if err == nil && string(buf[:n]) == "1" {
			log.Debug().Str("root", root).Str("xattr", attr).Msg("local backend: delegated subtree confirmed")
			return nil
		}
		if err == nil {
			err = fmt.Errorf("value %q", buf[:n])
		}
		errs = append(errs, fmt.Errorf("%s: %w", attr, err))
	}
	return fmt.Errorf("no delegate xattr on %s (%w)", root, errors.Join(errs...))
}

func writeCgroupKill(dir string) error {
	f, err := os.OpenFile(filepath.Join(dir, "cgroup.kill"), os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, err = f.WriteString("1")
	return errors.Join(err, f.Close())
}

// sweep tears down the wf-* cgroups a previous agent left under root. A
// process inside a workflow cgroup (a job running this code, such as a test)
// never sweeps: its siblings are other live workflows.
func (c *cgroupFS) sweep(root, own string) {
	if strings.HasPrefix(filepath.Base(own), wfCgroupPrefix) {
		log.Debug().Str("cgroup", own).Msg("local backend: running inside a workflow cgroup, not sweeping")
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		log.Error().Err(err).Str("root", root).Msg("local backend: cannot list workflow cgroups to sweep")
		return
	}
	for _, e := range entries {
		dir := filepath.Join(root, e.Name())
		if !e.IsDir() || !isWorkflowCgroup(root, dir) {
			continue
		}
		populated, _ := readPopulated(dir)
		if err := c.destroy(root, &workflowCgroup{dir: dir, fd: -1}); err != nil {
			log.Error().Err(err).Msg("local backend: leftover workflow cgroup could not be cleared")
			continue
		}
		log.Warn().Str("cgroup", dir).Bool("was_populated", populated).Msg("local backend: removed leftover workflow cgroup")
	}
}
