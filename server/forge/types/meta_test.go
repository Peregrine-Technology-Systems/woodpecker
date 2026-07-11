// Copyright 2023 Woodpecker Authors
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
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSortByName(t *testing.T) {
	fm := []*FileMeta{
		{
			Name: "a",
		},
		{
			Name: "c",
		},
		{
			Name: "b",
		},
	}

	assert.Equal(t, []*FileMeta{
		{
			Name: "a",
		},
		{
			Name: "b",
		},
		{
			Name: "c",
		},
	}, SortByName(fm))
}

func TestIsPipelineConfigFile(t *testing.T) {
	cases := map[string]bool{
		"pipeline.yaml":        true,
		"pipeline.yml":         true,
		".woodpecker.yaml":     true,
		"a.yaml":               true,
		"links.yaml.disabled":  false,
		"secscan.yml.disabled": false,
		"README.md":            false,
		"config.json":          false,
		"noext":                false,
		"":                     false,
	}
	for name, want := range cases {
		assert.Equalf(t, want, IsPipelineConfigFile(name), "IsPipelineConfigFile(%q)", name)
	}
}
