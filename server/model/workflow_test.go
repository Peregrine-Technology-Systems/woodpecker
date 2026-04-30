// Copyright 2026 Peregrine Technology Systems
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0

package model

import "testing"

func TestKilledByAgentDisconnect(t *testing.T) {
	cases := []struct {
		name     string
		workflow *Workflow
		want     bool
	}{
		{
			name:     "nil workflow",
			workflow: nil,
			want:     false,
		},
		{
			name:     "killed with disconnect signature",
			workflow: &Workflow{State: StatusKilled, Error: "agent disconnected"},
			want:     true,
		},
		{
			name:     "killed with disconnect signature embedded in larger message",
			workflow: &Workflow{State: StatusKilled, Error: "task failed: agent disconnected during step"},
			want:     true,
		},
		{
			name:     "killed with non-disconnect error (real failure)",
			workflow: &Workflow{State: StatusKilled, Error: "exit code 1"},
			want:     false,
		},
		{
			name:     "killed with empty error (manual cancel)",
			workflow: &Workflow{State: StatusKilled, Error: ""},
			want:     false,
		},
		{
			name:     "running, ignore",
			workflow: &Workflow{State: StatusRunning, Error: "agent disconnected"},
			want:     false,
		},
		{
			name:     "failure state with disconnect text in error (still don't suppress — failure is a real signal)",
			workflow: &Workflow{State: StatusFailure, Error: "agent disconnected"},
			want:     false,
		},
		{
			name:     "superseded with disconnect text (different state, still post)",
			workflow: &Workflow{State: StatusSuperseded, Error: "agent disconnected"},
			want:     false,
		},
		{
			name:     "success, ignore",
			workflow: &Workflow{State: StatusSuccess},
			want:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.workflow.KilledByAgentDisconnect()
			if got != tc.want {
				t.Errorf("KilledByAgentDisconnect() = %v, want %v", got, tc.want)
			}
		})
	}
}
