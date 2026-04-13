// Copyright 2024 Woodpecker Authors
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

package gcppubsub

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/plugin"
)

type recordedMessage struct {
	data  []byte
	attrs map[string]string
}

func testPublisher(source string) (*Publisher, *[]recordedMessage) {
	var messages []recordedMessage
	pub := func(_ context.Context, data []byte, attrs map[string]string) {
		messages = append(messages, recordedMessage{data: data, attrs: attrs})
	}
	return newPublisher(source, pub, nil), &messages
}

func sampleEvent(eventType plugin.EventType) plugin.PipelineEvent {
	return plugin.PipelineEvent{
		Type:       eventType,
		RepoID:     1,
		RepoName:   "org/repo",
		PipelineID: 10,
		Number:     42,
		Status:     model.StatusSuccess,
		Ref:        "refs/heads/main",
		Branch:     "main",
		Commit:     "a1b2c3d4e5f6",
		Author:     "alice",
		Message:    "feat: add feature\n\nDetailed description",
		Event:      "push",
		Timestamp:  time.Date(2026, 4, 5, 12, 0, 0, 0, time.UTC),
	}
}

func TestOnEventPublishesCorrectJSON(t *testing.T) {
	pub, messages := testPublisher("woodpecker-server")

	err := pub.OnEvent(context.Background(), sampleEvent(plugin.EventPipelineCompleted))
	require.NoError(t, err)
	require.Len(t, *messages, 1)

	var got pubsubEvent
	require.NoError(t, json.Unmarshal((*messages)[0].data, &got))

	assert.Equal(t, "1.0", got.SchemaVersion)
	assert.Equal(t, "pipeline.success", got.Type)
	assert.Equal(t, "woodpecker-server", got.Source)
	assert.Equal(t, "org/repo", got.Data.Repo)
	assert.Equal(t, int64(42), got.Data.Pipeline)
	assert.Equal(t, "success", got.Data.Status)
	assert.Equal(t, "main", got.Data.Branch)
	assert.Equal(t, "a1b2c3d4", got.Data.Commit)
	assert.Equal(t, "alice", got.Data.Author)
	assert.Equal(t, "feat: add feature", got.Data.Message)
	assert.Equal(t, "push", got.Data.Event)
}

func TestOnEventAttributes(t *testing.T) {
	pub, messages := testPublisher("woodpecker-server")

	err := pub.OnEvent(context.Background(), sampleEvent(plugin.EventPipelineFailed))
	require.NoError(t, err)

	attrs := (*messages)[0].attrs
	assert.Equal(t, "pipeline.failed", attrs["event_type"])
	assert.Equal(t, "woodpecker-server", attrs["source"])
	assert.Equal(t, "critical", attrs["severity"])
}

func TestEventTypeMapping(t *testing.T) {
	tests := []struct {
		input    plugin.EventType
		expected string
	}{
		{plugin.EventPipelineCreated, "pipeline.created"},
		{plugin.EventPipelinePending, "pipeline.pending"},
		{plugin.EventPipelineStarted, "pipeline.started"},
		{plugin.EventPipelineCompleted, "pipeline.success"},
		{plugin.EventPipelineFailed, "pipeline.failed"},
		{plugin.EventPipelineKilled, "pipeline.killed"},
		{plugin.EventPipelineSuperseded, "pipeline.superseded"},
		{plugin.EventStepCompleted, "step.completed"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.expected, mapEventType(tt.input), "mapping for %s", tt.input)
	}
}

func TestUnknownEventTypeFallsThrough(t *testing.T) {
	assert.Equal(t, "custom.event", mapEventType("custom.event"))
}

func TestSeverityMapping(t *testing.T) {
	tests := []struct {
		eventType string
		severity  string
	}{
		{"pipeline.failed", "critical"},
		{"pipeline.killed", "warning"},
		{"pipeline.superseded", "info"},
		{"pipeline.success", "info"},
		{"pipeline.created", "info"},
	}
	for _, tt := range tests {
		pub, messages := testPublisher("test")
		event := sampleEvent(plugin.EventPipelineCompleted)
		// Override event type to test severity via the mapped type
		event.Type = plugin.EventType(tt.eventType)
		if mapped, ok := eventTypeMap[event.Type]; ok {
			_ = mapped
		}
		_ = pub.OnEvent(context.Background(), event)
		got := (*messages)[0].attrs["severity"]
		assert.Equal(t, tt.severity, got, "severity for %s", tt.eventType)
	}
}

func TestCommitTruncation(t *testing.T) {
	pub, messages := testPublisher("test")
	event := sampleEvent(plugin.EventPipelineCreated)
	event.Commit = "abcdef1234567890"

	_ = pub.OnEvent(context.Background(), event)

	var got pubsubEvent
	_ = json.Unmarshal((*messages)[0].data, &got)
	assert.Equal(t, "abcdef12", got.Data.Commit)
}

func TestShortCommitUnchanged(t *testing.T) {
	pub, messages := testPublisher("test")
	event := sampleEvent(plugin.EventPipelineCreated)
	event.Commit = "abc"

	_ = pub.OnEvent(context.Background(), event)

	var got pubsubEvent
	_ = json.Unmarshal((*messages)[0].data, &got)
	assert.Equal(t, "abc", got.Data.Commit)
}

func TestMessageFirstLine(t *testing.T) {
	assert.Equal(t, "first", firstLine("first\nsecond", 80))
	assert.Equal(t, "short", firstLine("short", 80))
	assert.Equal(t, "12345", firstLine("1234567890", 5))
	assert.Equal(t, "12345", firstLine("1234567890\nmore", 5))
	assert.Equal(t, "", firstLine("", 80))
}

func TestName(t *testing.T) {
	pub, _ := testPublisher("test")
	assert.Equal(t, "gcppubsub", pub.Name())
}

func TestCloseCallsCloseFn(t *testing.T) {
	called := false
	pub := newPublisher("test", nil, func() error {
		called = true
		return nil
	})
	err := pub.Close()
	assert.NoError(t, err)
	assert.True(t, called)
}

func TestCloseReturnsError(t *testing.T) {
	pub := newPublisher("test", nil, func() error {
		return errors.New("close failed")
	})
	err := pub.Close()
	assert.EqualError(t, err, "close failed")
}

func TestCloseNilFn(t *testing.T) {
	pub := newPublisher("test", nil, nil)
	assert.NoError(t, pub.Close())
}

func TestAllEventTypesPublish(t *testing.T) {
	types := []plugin.EventType{
		plugin.EventPipelineCreated,
		plugin.EventPipelinePending,
		plugin.EventPipelineStarted,
		plugin.EventPipelineCompleted,
		plugin.EventPipelineFailed,
		plugin.EventPipelineKilled,
		plugin.EventPipelineSuperseded,
		plugin.EventStepCompleted,
	}
	for _, et := range types {
		pub, messages := testPublisher("test")
		err := pub.OnEvent(context.Background(), sampleEvent(et))
		assert.NoError(t, err, "OnEvent for %s", et)
		assert.Len(t, *messages, 1, "message count for %s", et)
	}
}

func TestTimestampFormat(t *testing.T) {
	pub, messages := testPublisher("test")
	event := sampleEvent(plugin.EventPipelineCreated)
	event.Timestamp = time.Date(2026, 4, 5, 14, 30, 45, 123456000, time.UTC)

	_ = pub.OnEvent(context.Background(), event)

	var got pubsubEvent
	_ = json.Unmarshal((*messages)[0].data, &got)
	assert.Equal(t, "2026-04-05T14:30:45.123456Z", got.Timestamp)
}
