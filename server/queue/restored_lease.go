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

package queue

import (
	"time"

	"go.woodpecker-ci.org/woodpecker/v3/shared/constant"
)

// defaultRestoredLease is the first lease of a task restored as running after
// a server restart (#427). The server has no evidence yet that its agent is
// alive, so it does not get the full TaskTimeout safety net: an agent that
// never comes back has its work released in about 6 minutes instead of 15.
//
// It must outlast the slowest renewal a live agent makes, or the restart
// itself would release live work: WS agents renew on every 10 s health
// report, but the gRPC agent's lease loop renews only every TaskTimeout/3
// (agent/runner.go). Hence TaskTimeout/3 plus a margin of TaskTimeout/15 (a
// minute at the default), not the shorter value the WS agents alone would
// allow. It scales with TaskTimeout, which tests shrink.
func defaultRestoredLease() time.Duration {
	return constant.TaskTimeout/3 + constant.TaskTimeout/15
}
