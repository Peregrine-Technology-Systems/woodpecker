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
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"go.woodpecker-ci.org/woodpecker/v3/pipeline/frontend/metadata"
)

// Coverage for the When helpers of constraint.go, which #435 touched.

func TestWhen_IsLocal(t *testing.T) {
	assert.True(t, parseConstraints(t, "").IsLocal())
	assert.True(t, parseConstraints(t, "{local: true}").IsLocal())
	assert.False(t, parseConstraints(t, "[{event: push}, {local: false}]").IsLocal())
}

func TestWhen_UnmarshalYAML_Errors(t *testing.T) {
	for _, conf := range []string{"just-a-string", "[{status: {a: b}}]", "{status: {a: b}}"} {
		w := new(When)
		assert.Error(t, yaml.Unmarshal([]byte(conf), w), conf)
	}
}

func TestWhen_MarshalYAML(t *testing.T) {
	out, err := When{}.MarshalYAML()
	require.NoError(t, err)
	assert.Nil(t, out)

	one, err := parseConstraints(t, "{event: push, local: true}").MarshalYAML()
	require.NoError(t, err)
	c, ok := one.(Constraint)
	require.True(t, ok)
	assert.True(t, c.Local.ValueOrDefault(true), "local true is dropped to the default")

	two, err := parseConstraints(t, "[{event: push}, {event: tag}]").MarshalYAML()
	require.NoError(t, err)
	assert.Len(t, two, 2)
}

func TestWhen_Match_ErrorsAndDefaults(t *testing.T) {
	m := metadata.Metadata{Curr: metadata.Pipeline{Event: metadata.EventPush}}

	_, err := parseConstraints(t, `{evaluate: "1 +"}`).Match(m, true, map[string]string{})
	assert.Error(t, err, "a bad evaluate expression is an error")

	_, err = parseConstraints(t, `{evaluate: 'int("x") == 1'}`).Match(m, true, map[string]string{})
	assert.Error(t, err, "an evaluate expression that fails at run time is an error")

	ok, err := parseConstraints(t, "").Match(m, true, nil)
	require.NoError(t, err)
	assert.True(t, ok, "an empty when matches by default")

	ok, err = parseConstraints(t, `{evaluate: 'CI_PIPELINE_EVENT == "push"'}`).Match(m, true, nil)
	require.NoError(t, err)
	assert.True(t, ok, "evaluate reads the metadata env when none is passed")

	cron := metadata.Metadata{Curr: metadata.Pipeline{Event: metadata.EventCron, Cron: "nightly"}}
	ok, err = parseConstraints(t, "{cron: weekly}").Match(cron, true, nil)
	require.NoError(t, err)
	assert.False(t, ok)
}
