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

package main

import (
	"context"
	"fmt"

	"github.com/rs/zerolog/log"
	"github.com/urfave/cli/v3"

	"go.woodpecker-ci.org/woodpecker/v3/server"
	"go.woodpecker-ci.org/woodpecker/v3/server/plugin"
	"go.woodpecker-ci.org/woodpecker/v3/server/plugin/gcppubsub"
)

// setupPlugins initializes the plugin registry, event bus, and
// instantiates any plugins requested via WOODPECKER_PLUGINS.
func setupPlugins(ctx context.Context, c *cli.Command) error {
	registry := plugin.NewRegistry()
	bus := plugin.NewEventBus(ctx)
	registry.SetEventBus(bus)

	pluginNames := c.StringSlice("plugins")
	for _, name := range pluginNames {
		if err := loadPlugin(ctx, name, c, registry); err != nil {
			return fmt.Errorf("failed to load plugin %q: %w", name, err)
		}
	}

	server.Config.Services.Plugins = registry

	if len(pluginNames) > 0 {
		log.Info().Strs("plugins", pluginNames).Msg("plugins loaded")
	} else {
		log.Debug().Msg("no plugins configured")
	}

	return nil
}

// loadPlugin instantiates a plugin by name and registers it with the registry.
func loadPlugin(ctx context.Context, name string, c *cli.Command, registry *plugin.Registry) error {
	switch name {
	case "gcppubsub":
		return loadGCPPubSub(ctx, c, registry)
	// Phase 4: case "status-api":
	// Phase 5: case "external-dispatch":
	default:
		return fmt.Errorf("unknown plugin: %s", name)
	}
}

func loadGCPPubSub(ctx context.Context, c *cli.Command, registry *plugin.Registry) error {
	project := c.String("plugin-gcppubsub-project")
	if project == "" {
		return fmt.Errorf("gcppubsub plugin requires WOODPECKER_PLUGIN_GCPPUBSUB_PROJECT")
	}

	topic := c.String("plugin-gcppubsub-topic")
	pub, err := gcppubsub.New(ctx, project, topic)
	if err != nil {
		return fmt.Errorf("gcppubsub: %w", err)
	}

	registry.RegisterEventHook(pub)
	log.Info().Str("project", project).Str("topic", topic).Msg("gcppubsub plugin loaded")
	return nil
}
