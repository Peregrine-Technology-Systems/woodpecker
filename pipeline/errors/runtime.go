// Copyright 2023 Woodpecker Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package errors

import (
	"errors"
	"fmt"
)

var (
	// ErrSkip is used as a return value when container execution should be
	// skipped at runtime. It is not returned as an error by any function.
	ErrSkip = errors.New("Skipped")

	// ErrCancel is used as a return value when the container execution receives
	// a cancellation signal from the context.
	ErrCancel = errors.New("Canceled")

	// ErrAgentShutdown is the WorkflowState.Error an agent reports when it
	// cancels an in-flight workflow because the AGENT ITSELF is terminating
	// (SIGTERM — spot preemption / systemd stop), as distinct from a
	// server-issued cancel (UI/API supersede/user, which reports ErrCancel /
	// "Canceled"). Both arrive over the wire as Canceled=true with an identical
	// payload otherwise, so the server cannot tell a recoverable preemption
	// from a deliberate cancel without this positive signal. The server uses it
	// to re-queue an idempotent, no-work-done workflow onto a fresh agent
	// instead of finalizing it killed (#275). It is carried on the existing
	// Error wire field — same mechanism as the "agent disconnected" signature
	// the server already matches — so no RPC schema change is required.
	ErrAgentShutdown = errors.New("agent shutdown")
)

// An ExitError reports an unsuccessful exit.
type ExitError struct {
	UUID string
	Code int
}

// Error returns the error message in string format.
func (e *ExitError) Error() string {
	return fmt.Sprintf("uuid=%s: exit code %d", e.UUID, e.Code)
}

// An OomError reports the process received an OOMKill from the kernel.
type OomError struct {
	UUID string
	Code int
}

// Error returns the error message in string format.
func (e *OomError) Error() string {
	return fmt.Sprintf("uuid=%s: received oom kill", e.UUID)
}
