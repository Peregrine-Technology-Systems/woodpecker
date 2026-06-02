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

package gcppubsub

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// #259: pipeline-status publishing to the ci-events bus is best-effort and
// async — a publish failure is logged but must never affect the pipeline. That
// is correct, but the bus is now the Slack replacement for CI status, so a
// SUSTAINED inability to publish silently stops consumers (Campfire, scaler,
// monitoring) from receiving status while every pipeline still goes green. The
// log line is not alert-able on its own; these counters are the positive
// broken-state counterpart, so monitoring can alert on
// `rate(woodpecker_pubsub_publish_failures_total[15m]) > 0` and compute a
// failure ratio against the published total.
var (
	pubsubPublishFailures = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "woodpecker",
		Name:      "pubsub_publish_failures_total",
		Help:      "Pipeline-status events that failed to publish to the ci-events bus after the async result resolved (#259).",
	})

	pubsubPublished = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "woodpecker",
		Name:      "pubsub_published_total",
		Help:      "Pipeline-status events that published successfully to the ci-events bus — the denominator for a publish failure ratio (#259).",
	})
)

// recordPublishFailure / recordPublishSuccess are the single mutation surfaces
// for the counters so tests can assert without reaching into promauto internals
// and so the structurally-untestable client.go adapter only needs to call a
// named function (it stays exempt from the per-file coverage gate).
func recordPublishFailure() { pubsubPublishFailures.Inc() }

func recordPublishSuccess() { pubsubPublished.Inc() }
