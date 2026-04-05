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
)

// setupPlugins initializes the plugin registry, event bus, and
// instantiates any plugins requested via WOODPECKER_PLUGINS.
func setupPlugins(ctx context.Context, c *cli.Command) error {
	registry := plugin.NewRegistry()
	bus := plugin.NewEventBus(ctx)
	registry.SetEventBus(bus)

	pluginNames := c.StringSlice("plugins")
	for _, name := range pluginNames {
		if err := loadPlugin(ctx, name, registry); err != nil {
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

// loadPlugin instantiates a plugin by name.
// Concrete plugin implementations are added in later phases.
func loadPlugin(_ context.Context, name string, _ *plugin.Registry) error {
	switch name {
	// Phase 3: case "gcppubsub":
	// Phase 4: case "status-api":
	// Phase 5: case "external-dispatch":
	default:
		return fmt.Errorf("unknown plugin: %s", name)
	}
}
