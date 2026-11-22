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

package pipeline

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #394: a labelled counter publishes nothing until its first increment, so
// "the reconciler never held" and "the metric is missing" looked the same on
// /metrics. Every reason must be scraped from boot, at 0 if nothing held yet.
// Reads the default registry, which is what /metrics serves.
func TestReconcileHeldTotal_EveryReasonPublishedFromBoot(t *testing.T) {
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)

	reasons := map[string]bool{}
	for _, f := range families {
		if f.GetName() != "woodpecker_reconcile_held_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "reason" {
					reasons[l.GetValue()] = true
				}
			}
		}
	}
	for _, reason := range []string{heldQueueRecheck, heldAgentLive, heldTreeUnreadable} {
		assert.True(t, reasons[reason], "reason %q must be published before any hold", reason)
	}
}
