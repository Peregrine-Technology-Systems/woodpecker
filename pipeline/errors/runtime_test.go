// Copyright 2026 Peregrine Technology Systems
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

package errors

import "testing"

func TestSentinelMessages(t *testing.T) {
	// These strings are part of the agent↔server contract: ErrCancel's message
	// is matched server-side, and ErrAgentShutdown's "agent shutdown" message
	// must stay in lockstep with server/model.agentShutdownSignature (#275).
	cases := []struct {
		err  error
		want string
	}{
		{ErrSkip, "Skipped"},
		{ErrCancel, "Canceled"},
		{ErrAgentShutdown, "agent shutdown"},
	}
	for _, tc := range cases {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("sentinel message = %q, want %q", got, tc.want)
		}
	}
}

func TestExitErrorError(t *testing.T) {
	e := &ExitError{UUID: "abc", Code: 7}
	if got, want := e.Error(), "uuid=abc: exit code 7"; got != want {
		t.Errorf("ExitError.Error() = %q, want %q", got, want)
	}
}

func TestOomErrorError(t *testing.T) {
	e := &OomError{UUID: "xyz", Code: 137}
	if got, want := e.Error(), "uuid=xyz: received oom kill"; got != want {
		t.Errorf("OomError.Error() = %q, want %q", got, want)
	}
}
