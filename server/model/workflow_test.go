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

func TestWorkflowStatePredicates(t *testing.T) {
	if (Workflow{}).TableName() != "workflows" {
		t.Errorf("TableName() = %q, want workflows", (Workflow{}).TableName())
	}
	running := []struct {
		state StatusValue
		want  bool
	}{
		{StatusPending, true}, {StatusRunning, true},
		{StatusSuccess, false}, {StatusKilled, false},
	}
	for _, tc := range running {
		if got := (&Workflow{State: tc.state}).Running(); got != tc.want {
			t.Errorf("Running(%s) = %v, want %v", tc.state, got, tc.want)
		}
	}
	failing := []struct {
		state StatusValue
		want  bool
	}{
		{StatusError, true}, {StatusKilled, true}, {StatusSuperseded, true}, {StatusFailure, true},
		{StatusSuccess, false}, {StatusRunning, false},
	}
	for _, tc := range failing {
		if got := (&Workflow{State: tc.state}).Failing(); got != tc.want {
			t.Errorf("Failing(%s) = %v, want %v", tc.state, got, tc.want)
		}
	}
	if IsThereRunningStage([]*Workflow{{State: StatusSuccess}, {State: StatusKilled}}) {
		t.Error("IsThereRunningStage with no running stage should be false")
	}
	if !IsThereRunningStage([]*Workflow{{State: StatusSuccess}, {State: StatusRunning}}) {
		t.Error("IsThereRunningStage with a running stage should be true")
	}
}

func TestCanceledByAgentShutdown(t *testing.T) {
	cases := []struct {
		name     string
		workflow *Workflow
		want     bool
	}{
		{"nil workflow", nil, false},
		{"killed with shutdown signature", &Workflow{State: StatusKilled, Error: "agent shutdown"}, true},
		{"killed with shutdown signature embedded", &Workflow{State: StatusKilled, Error: "workflow canceled: agent shutdown in progress"}, true},
		// "Canceled" is the server-issued / plain cancel — NOT an agent shutdown.
		{"killed with plain Canceled (server-issued)", &Workflow{State: StatusKilled, Error: "Canceled"}, false},
		// A disconnect is a different class and must not read as shutdown.
		{"killed with disconnect signature", &Workflow{State: StatusKilled, Error: "agent disconnected"}, false},
		{"killed with empty error", &Workflow{State: StatusKilled, Error: ""}, false},
		{"running with shutdown text, ignore", &Workflow{State: StatusRunning, Error: "agent shutdown"}, false},
		{"failure with shutdown text, ignore", &Workflow{State: StatusFailure, Error: "agent shutdown"}, false},
		{"success, ignore", &Workflow{State: StatusSuccess}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.workflow.CanceledByAgentShutdown()
			if got != tc.want {
				t.Errorf("CanceledByAgentShutdown() = %v, want %v", got, tc.want)
			}
		})
	}
}
