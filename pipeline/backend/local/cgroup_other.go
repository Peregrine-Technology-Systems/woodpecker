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

package local

import (
	"errors"
	"os/exec"
	"syscall"
)

// containmentSupported: on Darwin and Windows the backend runs steps as it
// always has; workflow cgroups are a Linux facility (#412).
const containmentSupported = false

func openCgroupDir(dir string) (int, error) {
	return syscall.Open(dir, syscall.O_RDONLY, 0)
}

func closeCgroupDir(fd int) { _ = syscall.Close(fd) }

func bornInCgroup(*exec.Cmd, *workflowCgroup) {}

func checkDelegatedSubtree(string, string) error {
	return errors.New("cgroups are not available on this OS")
}
