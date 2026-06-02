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
	"fmt"
	"time"

	"cloud.google.com/go/pubsub"
	"github.com/rs/zerolog/log"
)

// New creates a Publisher backed by a real GCP Pub/Sub client.
//
// This is the structurally-untestable adapter boundary (live pubsub client,
// topic publish settings, async result.Get). It is isolated here so the
// testable publisher logic in publisher.go stays under the per-file coverage
// gate; the publish/close behavior is exercised via newPublisher with injected
// fakes in the tests.
func New(ctx context.Context, project, topicName string) (*Publisher, error) {
	client, err := pubsub.NewClient(ctx, project)
	if err != nil {
		return nil, fmt.Errorf("pubsub client: %w", err)
	}

	topic := client.Topic(topicName)
	topic.PublishSettings.CountThreshold = 100
	topic.PublishSettings.DelayThreshold = 500 * time.Millisecond

	pub := func(_ context.Context, data []byte, attrs map[string]string) {
		result := topic.Publish(context.Background(), &pubsub.Message{
			Data:       data,
			Attributes: attrs,
		})
		go func() {
			if _, err := result.Get(context.Background()); err != nil {
				recordPublishFailure() // #259: alert-able via woodpecker_pubsub_publish_failures_total
				log.Warn().Err(err).Msg("pubsub publish failed")
				return
			}
			recordPublishSuccess()
		}()
	}

	cls := func() error {
		topic.Stop()
		return client.Close()
	}

	return newPublisher("woodpecker-server", pub, cls), nil
}
