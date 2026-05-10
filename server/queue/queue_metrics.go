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
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// #192: tasks that left the queue without ever being dispatched to an
// agent worker. Pre-#192 the only signal was a stuck pipeline; we
// couldn't tell whether dispatch was attempted, whether a worker was
// offered, or whether something else marked it killed before dispatch.
//
// Reasons (initial taxonomy — refine as we observe more failure modes
// in production):
//   - cancelled_before_dispatch     — task was pending or waitingOnDeps
//     and got removed by an external cancel (Cancel() → ErrorAtOnce()
//     → finished() → removeFromPendingAndWaiting). Correlates with
//     #190 mode B "killed/canceled before dispatch."
//   - dependency_unsatisfied_terminal — task is waiting for a dep that
//     just transitioned to a terminal failure status. The task itself
//     will never dispatch successfully.
var dispatchFailures = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "woodpecker",
	Name:      "pipeline_dispatch_failures_total",
	Help:      "Tasks that left the queue without being dispatched to an agent worker, by reason (#192).",
}, []string{"reason"})

// recordDispatchFailure increments the counter and is the single mutation
// surface so tests can assert on a single label set without reaching into
// promauto internals. Exported for use by the persistent-queue wrapper.
func recordDispatchFailure(reason string) {
	dispatchFailures.WithLabelValues(reason).Inc()
}
