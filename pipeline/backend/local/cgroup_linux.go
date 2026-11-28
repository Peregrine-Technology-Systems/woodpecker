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
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// containmentSupported: workflow cgroups are a Linux facility (#412).
const containmentSupported = true

func openCgroupDir(dir string) (int, error) {
	return unix.Open(dir, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
}

func closeCgroupDir(fd int) { _ = unix.Close(fd) }

// bornInCgroup makes the step's process start inside the workflow cgroup
// (clone3 with CLONE_INTO_CGROUP), so there is no moment it sits elsewhere.
func bornInCgroup(cmd *exec.Cmd, wc *workflowCgroup) {
	if wc == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.UseCgroupFD = true
	cmd.SysProcAttr.CgroupFD = wc.fd
}

// checkDelegatedSubtree applies checkDelegation with the real xattr call.
func checkDelegatedSubtree(root, leaf string) error {
	return checkDelegation(root, leaf, unix.Getxattr)
}
