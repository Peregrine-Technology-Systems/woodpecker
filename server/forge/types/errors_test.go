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

package types

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestErrTransientForge(t *testing.T) {
	underlying := errors.New("github 503")
	err := &ErrTransientForge{Err: underlying}

	// hook.go classifies via errors.Is against a zero-value sentinel.
	assert.ErrorIs(t, err, &ErrTransientForge{}, "matches the sentinel by type")
	// The underlying cause stays reachable for logging/further inspection.
	assert.ErrorIs(t, err, underlying, "unwraps to the underlying cause")
	assert.ErrorContains(t, err, "github 503")

	// A different error type must not be mistaken for a transient forge error.
	assert.NotErrorIs(t, errors.New("malformed payload"), &ErrTransientForge{})

	// Nil-cause variant is still a valid, non-panicking sentinel.
	assert.ErrorIs(t, &ErrTransientForge{}, &ErrTransientForge{})
	assert.Equal(t, "transient forge error", (&ErrTransientForge{}).Error())
}

func TestErrIgnoreEvent(t *testing.T) {
	withReason := &ErrIgnoreEvent{Event: "push", Reason: "branch filtered"}
	assert.ErrorIs(t, withReason, &ErrIgnoreEvent{})
	assert.Equal(t, "explicit ignored event 'push', reason: branch filtered", withReason.Error())

	noReason := &ErrIgnoreEvent{Event: "push"}
	assert.Equal(t, "explicit ignored event 'push'", noReason.Error())

	assert.NotErrorIs(t, errors.New("other"), &ErrIgnoreEvent{})
}

func TestErrConfigNotFound(t *testing.T) {
	err := &ErrConfigNotFound{Configs: []string{".woodpecker.yml", ".woodpecker.yaml"}}
	assert.ErrorIs(t, err, &ErrConfigNotFound{})
	assert.Equal(t, "configs not found: .woodpecker.yml, .woodpecker.yaml", err.Error())

	assert.NotErrorIs(t, errors.New("other"), &ErrConfigNotFound{})
}
