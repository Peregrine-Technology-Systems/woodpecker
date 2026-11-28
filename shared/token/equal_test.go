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

package token

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// #419: secrets are compared in constant time. Timing itself is not
// unit-testable; these pin the semantics every caller relies on, in
// particular that an empty configured secret never matches.
func TestEqual(t *testing.T) {
	for name, tc := range map[string]struct {
		got, want string
		eq        bool
	}{
		"same":                     {got: "s3cret-token", want: "s3cret-token", eq: true},
		"different, same length":   {got: "s3cret-tokeX", want: "s3cret-token"},
		"prefix of the secret":     {got: "s3cret", want: "s3cret-token"},
		"longer than the secret":   {got: "s3cret-token-and-more", want: "s3cret-token"},
		"empty presented":          {got: "", want: "s3cret-token"},
		"empty configured":         {got: "anything", want: ""},
		"both empty never matches": {got: "", want: ""},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.eq, Equal(tc.got, tc.want))
		})
	}
}
