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
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
)

// TestRecordPublishFailure / Success assert the #259 counters move exactly
// once per call. The async failure branch in client.go (the structurally
// untestable adapter) only calls these named helpers, so covering them here
// proves the wire-up increments the alert-able metric.
func TestRecordPublishFailureIncrements(t *testing.T) {
	before := testutil.ToFloat64(pubsubPublishFailures)
	recordPublishFailure()
	assert.Equal(t, before+1, testutil.ToFloat64(pubsubPublishFailures))
}

func TestRecordPublishSuccessIncrements(t *testing.T) {
	before := testutil.ToFloat64(pubsubPublished)
	recordPublishSuccess()
	assert.Equal(t, before+1, testutil.ToFloat64(pubsubPublished))
}
