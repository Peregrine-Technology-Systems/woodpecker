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

package model

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// #388: every seat shares one forge login, so CanceledByUser cannot say WHO
// cancelled. The actor is self-declared by the caller. It is validated strictly
// and REJECTED when malformed rather than normalised: silently lowercasing or
// trimming would store a value the caller never sent, and a dropped value would
// make an attributed cancel look unattributed.
func TestParseCancelActor(t *testing.T) {
	valid := []string{
		"seat.woodpecker", "scaler.stale", "healthcheck", "preemption-handler",
		"zombie_sweeper", "ab", strings.Repeat("a", 64),
	}
	for _, in := range valid {
		t.Run("valid/"+in, func(t *testing.T) {
			got, err := ParseCancelActor(in)
			assert.NoError(t, err)
			assert.Equal(t, in, got)
		})
	}

	t.Run("empty means not declared, not an error", func(t *testing.T) {
		got, err := ParseCancelActor("")
		assert.NoError(t, err)
		assert.Equal(t, "", got)
	})

	invalid := map[string]string{
		"uppercase":         "Seat.woodpecker",
		"space":             "seat woodpecker",
		"leading space":     " seat",
		"trailing space":    "seat ",
		"newline injection": "seat\nINFO forged log line",
		"slash":             "seat/woodpecker",
		"quote":             `seat"x`,
		"unicode":           "séat",
		"single char":       "a",
		"leading digit":     "1seat",
		"leading dot":       ".seat",
		"too long":          strings.Repeat("a", 65),
		"tab":               "seat\tx",
		"null byte":         "seat\x00x",
		"only whitespace":   "  ",
	}
	for name, in := range invalid {
		t.Run("invalid/"+name, func(t *testing.T) {
			got, err := ParseCancelActor(in)
			assert.ErrorIs(t, err, ErrInvalidCancelActor)
			assert.Equal(t, "", got, "a rejected value must not leak out")
		})
	}
}
