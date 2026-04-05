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
	"strings"
	"time"

	"go.woodpecker-ci.org/woodpecker/v3/server/plugin"
)

const schemaVersion = "1.0"

// pubsubEvent matches the webhook sidecar's JSON schema (schema_version "1.0").
type pubsubEvent struct {
	SchemaVersion string     `json:"schema_version"`
	Type          string     `json:"type"`
	Source        string     `json:"source"`
	Timestamp     string     `json:"timestamp"`
	Data          pubsubData `json:"data"`
}

type pubsubData struct {
	Repo     string `json:"repo"`
	Pipeline int64  `json:"pipeline"`
	Status   string `json:"status"`
	Branch   string `json:"branch"`
	Commit   string `json:"commit"`
	Author   string `json:"author"`
	Message  string `json:"message"`
	Event    string `json:"event"`
}

// eventTypeMap maps internal event types to sidecar-compatible Pub/Sub types.
// Key difference: EventPipelineCompleted maps to "pipeline.success".
var eventTypeMap = map[plugin.EventType]string{
	plugin.EventPipelineCreated:   "pipeline.created",
	plugin.EventPipelinePending:   "pipeline.pending",
	plugin.EventPipelineStarted:   "pipeline.started",
	plugin.EventPipelineCompleted: "pipeline.success",
	plugin.EventPipelineFailed:    "pipeline.failed",
	plugin.EventPipelineKilled:    "pipeline.killed",
	plugin.EventStepCompleted:     "step.completed",
}

var severityMap = map[string]string{
	"pipeline.created": "info",
	"pipeline.pending": "info",
	"pipeline.started": "info",
	"pipeline.success": "info",
	"pipeline.failed":  "critical",
	"pipeline.killed":  "warning",
	"step.completed":   "info",
}

func buildEvent(source string, event plugin.PipelineEvent) pubsubEvent {
	return pubsubEvent{
		SchemaVersion: schemaVersion,
		Type:          mapEventType(event.Type),
		Source:        source,
		Timestamp:     event.Timestamp.UTC().Format(time.RFC3339Nano),
		Data: pubsubData{
			Repo:     event.RepoName,
			Pipeline: event.Number,
			Status:   string(event.Status),
			Branch:   event.Branch,
			Commit:   truncate(event.Commit, 8),
			Author:   event.Author,
			Message:  firstLine(event.Message, 80),
			Event:    event.Event,
		},
	}
}

func mapEventType(t plugin.EventType) string {
	if mapped, ok := eventTypeMap[t]; ok {
		return mapped
	}
	return string(t)
}

func firstLine(s string, maxLen int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return truncate(s, maxLen)
}

func truncate(s string, maxLen int) string {
	if len(s) > maxLen {
		return s[:maxLen]
	}
	return s
}
