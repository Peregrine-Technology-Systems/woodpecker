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
	"fmt"
	"strings"

	"go.woodpecker-ci.org/woodpecker/v3/server/plugin"
)

// ErrEmptyEventType is returned by OnEvent when the resolved wire type is
// empty. Fail closed: a typeless event must never reach the ci-events topic,
// where subscribers route on the type. See woodpecker#264.
var ErrEmptyEventType = errors.New("gcppubsub: refusing to publish event with empty type")

// publishFn abstracts Pub/Sub publishing for testability.
type publishFn func(ctx context.Context, data []byte, attrs map[string]string)

// marshal is a seam over json.Marshal so the marshal-error branch in OnEvent is
// reachable from tests. The flat pubsubEvent struct cannot fail to marshal in
// production, but the error handling is kept (idiomatic) and must be covered.
var marshal = json.Marshal

// Publisher implements plugin.EventHook by publishing events to GCP Pub/Sub.
// Event format matches the webhook sidecar's schema_version "1.0" for
// drop-in replacement compatibility.
type Publisher struct {
	publish publishFn
	close   func() error
	source  string
}

// newPublisher creates a Publisher with injectable dependencies (for testing).
func newPublisher(source string, pub publishFn, cls func() error) *Publisher {
	return &Publisher{publish: pub, close: cls, source: source}
}

func (p *Publisher) Name() string { return "gcppubsub" }

// OnEvent formats the event as sidecar-compatible JSON and publishes to Pub/Sub.
func (p *Publisher) OnEvent(ctx context.Context, event plugin.PipelineEvent) error {
	pe := buildEvent(p.source, event)

	if strings.TrimSpace(pe.Type) == "" {
		return fmt.Errorf("%w (source=%q, pipeline=%d)", ErrEmptyEventType, p.source, event.Number)
	}

	data, err := marshal(pe)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}

	attrs := map[string]string{
		"event_type": pe.Type,
		"source":     p.source,
		"severity":   severityMap[pe.Type],
	}
	if attrs["severity"] == "" {
		attrs["severity"] = "info"
	}

	p.publish(ctx, data, attrs)
	return nil
}

// Close flushes pending messages and closes the Pub/Sub client.
func (p *Publisher) Close() error {
	if p.close != nil {
		return p.close()
	}
	return nil
}
