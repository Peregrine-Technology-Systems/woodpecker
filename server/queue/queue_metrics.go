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

// #243: orphaned-workflow observability. The dispatcher samples this every
// process() tick. Two manifestations, because static analysis of the #243
// incident could not distinguish them and we want the metric to catch either:
//
//   - running_dead_owner   — a task in q.running whose owning agent has been
//     POSITIVELY observed as disconnected (its WS reconnect grace expired —
//     recycled / preempted / came back under a new agent_id per #77). These are
//     reclaimed within one tick via the owner-liveness path rather than waiting
//     the 15-minute TaskTimeout lease. #246: an agent whose liveness was never
//     tracked (gRPC/local-backend) is NOT counted here and is never reclaimed.
//   - pending_dispatchable — a task in q.pending that ShouldRun() (deps
//     satisfied) yet has sat undispatched past orphanAgeThreshold. Indicates
//     no agent matched its labels, or a dispatch-loop stall.
//
// Naming: this is an instantaneous count, so it is a Gauge (no _total
// suffix). The cumulative reclaim count is the separate _reclaimed_total
// counter below. Alert on `woodpecker_orphaned_workflows > 0` sustained for
// several minutes — that is the "confused operator" case from #243 made
// alert-able.
var orphanedWorkflows = promauto.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "woodpecker",
	Name:      "orphaned_workflows",
	Help:      "Workflows the dispatcher considers orphaned right now, by manifestation: running_dead_owner / pending_dispatchable (#243).",
}, []string{"state"})

// orphanReclaimsTotal counts owner-liveness reclaims actually triggered: a
// running task's agent was found disconnected and ReleaseAgentTasks was
// re-fired without waiting the TaskTimeout lease. A non-zero rate is the
// actionable signal that agent recycles are stranding tasks. (#243)
var orphanReclaimsTotal = promauto.NewCounter(prometheus.CounterOpts{
	Namespace: "woodpecker",
	Name:      "orphaned_workflows_reclaimed_total",
	Help:      "Cumulative owner-liveness reclaims: a running task's agent was found disconnected and its tasks were released early (#243).",
})

// setOrphanedWorkflows is the single mutation surface for the gauge so tests
// can assert without reaching into promauto internals.
func setOrphanedWorkflows(state string, n float64) {
	orphanedWorkflows.WithLabelValues(state).Set(n)
}

// recordOrphanReclaim increments the reclaim counter.
func recordOrphanReclaim() {
	orphanReclaimsTotal.Inc()
}
