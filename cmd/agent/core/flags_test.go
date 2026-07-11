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

package core

import (
	"testing"

	"github.com/urfave/cli/v3"

	"go.woodpecker-ci.org/woodpecker/v3/shared/constant"
)

// TestKeepaliveFlagsWired guards the woodpecker#312 flag-name regression: the gRPC
// dial in agent.go reads "keepalive-time"/"keepalive-timeout"; those flags must
// exist (they previously read "grpc-keepalive-time", defined nowhere, so the dial
// silently got 0), and keepalive-time must default to the coordinated constant so
// it stays >= the server's keepalive-min-time floor.
func TestKeepaliveFlagsWired(t *testing.T) {
	byName := map[string]cli.Flag{}
	for _, f := range flags {
		for _, n := range f.Names() {
			byName[n] = f
		}
	}

	for _, name := range []string{"keepalive-time", "keepalive-timeout"} {
		if _, ok := byName[name]; !ok {
			t.Fatalf("agent flag %q is not defined, but agent.go dials with it (woodpecker#312)", name)
		}
	}

	df, ok := byName["keepalive-time"].(*cli.DurationFlag)
	if !ok {
		t.Fatalf("keepalive-time is not a DurationFlag")
	}
	if df.Value != constant.GRPCKeepaliveTime {
		t.Fatalf("keepalive-time default = %v, want %v (must track the coordinated keepalive constant)", df.Value, constant.GRPCKeepaliveTime)
	}
}
