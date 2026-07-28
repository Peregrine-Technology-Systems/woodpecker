// Copyright 2026 Peregrine Technology Systems
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

package logdrain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
)

func TestNewDisabledWhenNoProject(t *testing.T) {
	// Empty project → disabled no-op, never touches GCP, never errors.
	d := New(context.Background(), "", "")
	assert.NotNil(t, d)
	assert.False(t, d.Enabled(), "empty project yields a disabled drain")
	assert.NoError(t, d.Close(), "disabled drain closes cleanly")
}

// captureLogs swaps log.Logger to a buffer-backed JSON logger for the
// duration of one test. Same idiom as server/pipeline/cancel_test.go.
func captureLogs(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := log.Logger
	log.Logger = zerolog.New(buf).With().Logger()
	return buf, func() { log.Logger = prev }
}

// onErrorLogLines scans captured output for the drain's write-error log line
// and returns each occurrence, parsed, in order.
func onErrorLogLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" || !strings.Contains(line, "Cloud Logging write error") {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("could not parse log line %q: %v", line, err)
		}
		records = append(records, rec)
	}
	return records
}

// #333: a write failure must be LOUD (ERROR, not the WARN it silently shipped
// at before), and consecutive failures must be distinguishable from a single
// blip via a running count — proof the fix actually escalates, not just that
// it compiles.
func TestOnErrorHandlerEscalatesLoudlyWithRunningCount(t *testing.T) {
	buf, restore := captureLogs(t)
	defer restore()

	handler := newOnErrorHandler()
	handler(errors.New("boom 1"))
	handler(errors.New("boom 2"))

	records := onErrorLogLines(t, buf)
	if assert.Len(t, records, 2, "both failures logged") {
		assert.Equal(t, "error", records[0]["level"], "first failure is ERROR, not WARN — silent-OK is what shipped before #333")
		assert.Equal(t, logDrainWriteErrorType, records[0]["type"], "stable type field — filterable/alertable without string-matching Msg()")
		assert.Equal(t, float64(1), records[0]["consecutive_failures"])
		assert.Equal(t, "boom 1", records[0]["error"])

		assert.Equal(t, "error", records[1]["level"])
		assert.Equal(t, logDrainWriteErrorType, records[1]["type"])
		assert.Equal(t, float64(2), records[1]["consecutive_failures"], "count keeps rising so a systemic outage reads differently from one blip")
		assert.Equal(t, "boom 2", records[1]["error"])
	}
}

// Two independent handlers (as New() would build per-Drain) must not share
// state — a fresh drain shouldn't inherit another drain's failure count.
func TestOnErrorHandlerCountIsPerHandlerInstance(t *testing.T) {
	buf, restore := captureLogs(t)
	defer restore()

	h1 := newOnErrorHandler()
	h2 := newOnErrorHandler()
	h1(errors.New("h1 boom"))
	h2(errors.New("h2 boom"))

	records := onErrorLogLines(t, buf)
	if assert.Len(t, records, 2) {
		assert.Equal(t, float64(1), records[0]["consecutive_failures"], "h1's first failure")
		assert.Equal(t, float64(1), records[1]["consecutive_failures"], "h2's first failure — independent counter, not shared with h1")
	}
}
