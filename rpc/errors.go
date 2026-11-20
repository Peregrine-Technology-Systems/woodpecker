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

package rpc

import "errors"

// ErrStepTerminal is returned to the agent when the server rejected a step
// update because the step is already in a terminal state (#31, #394). It is a
// permanent answer: the server has abandoned that work, so re-sending the same
// update can never succeed. Clients must not retry it.
var ErrStepTerminal = errors.New("step is in terminal state on the server")

// AckCodeStepTerminal is the typed code a WebSocket ack carries for
// ErrStepTerminal, so the agent classifies the rejection by type and never by
// matching the error text (#394).
const AckCodeStepTerminal = "step_terminal"
