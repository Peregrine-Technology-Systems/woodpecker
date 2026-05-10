// Copyright 2026 Woodpecker Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0

package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPipeline_TableName(t *testing.T) {
	assert.Equal(t, "pipelines", Pipeline{}.TableName())
}

func TestPipeline_IsMultiPipeline(t *testing.T) {
	cases := []struct {
		name string
		pl   Pipeline
		want bool
	}{
		{"no workflows", Pipeline{}, false},
		{"single workflow", Pipeline{Workflows: []*Workflow{{}}}, false},
		{"multi workflows", Pipeline{Workflows: []*Workflow{{}, {}}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.pl.IsMultiPipeline())
		})
	}
}

func TestPipeline_IsPullRequest(t *testing.T) {
	cases := map[WebhookEvent]bool{
		EventPull:         true,
		EventPullClosed:   true,
		EventPullMetadata: true,
		EventPush:         false,
		EventTag:          false,
		EventCron:         false,
		EventManual:       false,
		EventRelease:      false,
		EventDeploy:       false,
	}
	for event, want := range cases {
		t.Run(string(event), func(t *testing.T) {
			pl := Pipeline{Event: event}
			assert.Equal(t, want, pl.IsPullRequest())
		})
	}
}
