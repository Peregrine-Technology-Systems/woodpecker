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
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	prometheus_auto "github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog/log"

	"go.woodpecker-ci.org/woodpecker/v3/server"
	"go.woodpecker-ci.org/woodpecker/v3/server/store"
)

func startMetricsCollector(ctx context.Context, _store store.Store) {
	pendingSteps := prometheus_auto.NewGauge(prometheus.GaugeOpts{
		Namespace: "woodpecker",
		Name:      "pending_steps",
		Help:      "Total number of pending pipeline steps.",
	})
	ciPendingSteps := prometheus_auto.NewGauge(prometheus.GaugeOpts{
		Namespace: "ci",
		Name:      "pending_steps",
		Help:      "Total number of pending pipeline steps (ci_* alias for woodpecker_pending_steps, #173).",
	})
	waitingSteps := prometheus_auto.NewGauge(prometheus.GaugeOpts{
		Namespace: "woodpecker",
		Name:      "waiting_steps",
		Help:      "Total number of pipeline waiting on deps.",
	})
	ciWaitingSteps := prometheus_auto.NewGauge(prometheus.GaugeOpts{
		Namespace: "ci",
		Name:      "waiting_steps",
		Help:      "Total number of pipeline waiting on deps (ci_* alias for woodpecker_waiting_steps, #173).",
	})
	runningSteps := prometheus_auto.NewGauge(prometheus.GaugeOpts{
		Namespace: "woodpecker",
		Name:      "running_steps",
		Help:      "Total number of running pipeline steps.",
	})
	ciRunningSteps := prometheus_auto.NewGauge(prometheus.GaugeOpts{
		Namespace: "ci",
		Name:      "running_steps",
		Help:      "Total number of running pipeline steps (ci_* alias for woodpecker_running_steps, #173).",
	})
	workers := prometheus_auto.NewGauge(prometheus.GaugeOpts{
		Namespace: "woodpecker",
		Name:      "worker_count",
		Help:      "Total number of workers.",
	})
	ciWorkers := prometheus_auto.NewGauge(prometheus.GaugeOpts{
		Namespace: "ci",
		Name:      "worker_count",
		Help:      "Total number of workers (ci_* alias for woodpecker_worker_count, #173).",
	})
	pipelines := prometheus_auto.NewGauge(prometheus.GaugeOpts{
		Namespace: "woodpecker",
		Name:      "pipeline_total_count",
		Help:      "Total number of pipelines.",
	})
	ciPipelines := prometheus_auto.NewGauge(prometheus.GaugeOpts{
		Namespace: "ci",
		Name:      "pipeline_total_count",
		Help:      "Total number of pipelines (ci_* alias for woodpecker_pipeline_total_count, #173).",
	})
	users := prometheus_auto.NewGauge(prometheus.GaugeOpts{
		Namespace: "woodpecker",
		Name:      "user_count",
		Help:      "Total number of users.",
	})
	ciUsers := prometheus_auto.NewGauge(prometheus.GaugeOpts{
		Namespace: "ci",
		Name:      "user_count",
		Help:      "Total number of users (ci_* alias for woodpecker_user_count, #173).",
	})
	repos := prometheus_auto.NewGauge(prometheus.GaugeOpts{
		Namespace: "woodpecker",
		Name:      "repo_count",
		Help:      "Total number of repos.",
	})
	ciRepos := prometheus_auto.NewGauge(prometheus.GaugeOpts{
		Namespace: "ci",
		Name:      "repo_count",
		Help:      "Total number of repos (ci_* alias for woodpecker_repo_count, #173).",
	})

	go func() {
		log.Info().Msg("queue metric collector started")

		for {
			stats := server.Config.Services.Queue.Info(ctx)
			pendingSteps.Set(float64(stats.Stats.Pending))
			ciPendingSteps.Set(float64(stats.Stats.Pending))
			waitingSteps.Set(float64(stats.Stats.WaitingOnDeps))
			ciWaitingSteps.Set(float64(stats.Stats.WaitingOnDeps))
			runningSteps.Set(float64(stats.Stats.Running))
			ciRunningSteps.Set(float64(stats.Stats.Running))
			workers.Set(float64(stats.Stats.Workers))
			ciWorkers.Set(float64(stats.Stats.Workers))

			select {
			case <-ctx.Done():
				log.Info().Msg("queue metric collector stopped")
				return
			case <-time.After(queueInfoRefreshInterval):
			}
		}
	}()
	go func() {
		log.Info().Msg("store metric collector started")

		for {
			repoCount, repoErr := _store.GetRepoCount()
			userCount, userErr := _store.GetUserCount()
			pipelineCount, pipelineErr := _store.GetPipelineCount()
			pipelines.Set(float64(pipelineCount))
			ciPipelines.Set(float64(pipelineCount))
			users.Set(float64(userCount))
			ciUsers.Set(float64(userCount))
			repos.Set(float64(repoCount))
			ciRepos.Set(float64(repoCount))

			if err := errors.Join(repoErr, userErr, pipelineErr); err != nil {
				log.Error().Err(err).Msg("could not update store information for metrics")
			}

			select {
			case <-ctx.Done():
				log.Info().Msg("store metric collector stopped")
				return
			case <-time.After(storeInfoRefreshInterval):
			}
		}
	}()
}
