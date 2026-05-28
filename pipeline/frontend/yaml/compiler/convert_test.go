// Copyright 2024 Woodpecker Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package compiler

import (
	"testing"

	"github.com/stretchr/testify/assert"

	backend_types "go.woodpecker-ci.org/woodpecker/v3/pipeline/backend/types"
	yaml_types "go.woodpecker-ci.org/woodpecker/v3/pipeline/frontend/yaml/types"
)

func TestConvertVerify(t *testing.T) {
	t.Run("kind: deploy shorthand expands to a verify proof-query", func(t *testing.T) {
		got := convertVerify(&yaml_types.Container{
			Kind:               "deploy",
			VerifyURL:          "https://target/version",
			VerifyExpectCommit: "abcdef0",
		})
		assert.Equal(t, &backend_types.VerifyConfig{
			URL:          "https://target/version",
			ExpectCommit: "abcdef0",
		}, got)
	})

	t.Run("explicit verify block is carried through when when_killed is set", func(t *testing.T) {
		got := convertVerify(&yaml_types.Container{
			Verify: &yaml_types.VerifyConfig{
				WhenKilled:   true,
				URL:          "https://target/health",
				ExpectStatus: 204,
			},
		})
		assert.Equal(t, &backend_types.VerifyConfig{
			URL:          "https://target/health",
			ExpectStatus: 204,
		}, got)
	})

	t.Run("kind: deploy without verify_url yields no verification", func(t *testing.T) {
		assert.Nil(t, convertVerify(&yaml_types.Container{Kind: "deploy"}))
	})

	t.Run("verify block without when_killed yields no verification", func(t *testing.T) {
		assert.Nil(t, convertVerify(&yaml_types.Container{
			Verify: &yaml_types.VerifyConfig{URL: "https://target/version"},
		}))
	})

	t.Run("no verify config yields nil", func(t *testing.T) {
		assert.Nil(t, convertVerify(&yaml_types.Container{}))
	})
}

func TestConvertPortNumber(t *testing.T) {
	portDef := "1234"
	actualPort, err := convertPort(portDef)
	assert.NoError(t, err)
	assert.Equal(t, backend_types.Port{
		Number:   1234,
		Protocol: "",
	}, actualPort)
}

func TestConvertPortUdp(t *testing.T) {
	portDef := "1234/udp"
	actualPort, err := convertPort(portDef)
	assert.NoError(t, err)
	assert.Equal(t, backend_types.Port{
		Number:   1234,
		Protocol: "udp",
	}, actualPort)
}

func TestConvertPortWrongOrder(t *testing.T) {
	portDef := "tcp/1234"
	_, err := convertPort(portDef)
	assert.Error(t, err)
}

func TestConvertPortWrongDelimiter(t *testing.T) {
	portDef := "1234|udp"
	_, err := convertPort(portDef)
	assert.Error(t, err)
}

func TestConvertPortWrong(t *testing.T) {
	portDef := "http"
	_, err := convertPort(portDef)
	assert.Error(t, err)
}
