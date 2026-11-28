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

package constraint

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.woodpecker-ci.org/woodpecker/v3/pipeline/frontend/metadata"
)

// #435 part 2: `killed` is an opt-in status, never implied by an empty when or
// by success/failure.
func TestConstraintStatusKilled(t *testing.T) {
	testdata := []struct {
		conf string
		want bool
	}{
		{conf: "", want: false},
		{conf: "{status: [success, failure]}", want: false},
		{conf: "{status: [failure]}", want: false},
		{conf: "{status: [killed]}", want: true},
		{conf: "{status: [success, failure, killed]}", want: true},
		{conf: "{event: push, status: [killed]}", want: false},
		{conf: "{event: pull_request, status: [killed]}", want: true},
	}
	for _, test := range testdata {
		c := parseConstraints(t, test.conf)
		got := c.IncludesStatusKilled(metadata.Metadata{Curr: metadata.Pipeline{Event: metadata.EventPull}}, true, map[string]string{})
		assert.Equal(t, test.want, got, "when: '%s'", test.conf)
	}
}
