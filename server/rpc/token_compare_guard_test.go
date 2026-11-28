// Copyright 2026 Woodpecker Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package grpc

import (
	"os"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
)

// #419: the agent secret is compared in constant time (token.Equal), never with
// == or !=, which returns early at the first differing byte. Timing cannot be
// unit-tested, so this pins the source instead: it fails if a plain comparison
// against the secret comes back.
func TestAgentSecretIsNotComparedWithEquality(t *testing.T) {
	src, err := os.ReadFile("auth_server.go")
	require.NoError(t, err)
	plain := regexp.MustCompile(`[!=]=\s*s\.agentMasterToken\b|s\.agentMasterToken\s*[!=]=\s*[^"\s]`)
	require.Empty(t, plain.FindAllString(string(src), -1), "compare the agent secret with token.Equal")
}
