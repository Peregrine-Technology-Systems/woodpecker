// Copyright 2022 Woodpecker Authors
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

package grpc

import (
	"maps"
	"strings"
	"sync"
	"time"

	pipelineConsts "go.woodpecker-ci.org/woodpecker/v3/pipeline"
	"go.woodpecker-ci.org/woodpecker/v3/rpc"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/queue"
)

// deployHoldWindow is how long spot agents wait before accepting deploy tasks,
// giving on-demand agents time to boot and claim the job.
const deployHoldWindow = 30 * time.Second

// deployFirstSeen tracks when a deploy task was first evaluated.
// After deployHoldWindow, spot agents are allowed to take it.
var (
	deployFirstSeen   = make(map[string]time.Time)
	deployFirstSeenMu sync.Mutex
)

// deployTierBoost maps agent tier labels to score boosts for deploy workflows.
// Higher boost = preferred tier. Agents without a tier label get no boost.
var deployTierBoost = map[string]int{
	"ondemand": 20, // non-preemptible, best for deploys
	"n2":       15, // backup on-demand, second choice
	// "spot" gets no boost — default fallback
}

func createFilterFunc(agentFilter rpc.Filter) queue.FilterFn {
	return createFilterFuncWithDeploy(agentFilter, nil)
}

func createFilterFuncWithDeploy(agentFilter rpc.Filter, deployPatterns []string) queue.FilterFn {
	return func(task *model.Task) (bool, int) {
		// Create a copy of the labels for filtering to avoid modifying the original task
		labels := maps.Clone(task.Labels)

		if requiredLabelsMissing(labels, agentFilter.Labels) {
			return false, 0
		}

		// ignore internal labels for filtering
		for k := range labels {
			if strings.HasPrefix(k, pipelineConsts.InternalLabelPrefix) {
				delete(labels, k)
			}
		}

		score := 0
		for taskLabel, taskLabelValue := range labels {
			// if a task label is empty it will be ignored
			if taskLabelValue == "" {
				continue
			}

			// all task labels are required to be present for an agent to match
			agentLabelValue, ok := agentFilter.Labels[taskLabel]
			if !ok {
				// Check for required label
				agentLabelValue, ok = agentFilter.Labels["!"+taskLabel]
				if !ok {
					return false, 0
				}
			}

			switch agentLabelValue {
			// if agent label has a wildcard
			case "*":
				score++
			// if agent label has an exact match
			case taskLabelValue:
				score += 10
			// agent doesn't match
			default:
				return false, 0
			}
		}

		// Deploy auto-routing: boost on-demand/n2 agents and hold
		// deploy tasks from spot agents for deployHoldWindow seconds.
		if len(deployPatterns) > 0 && isDeployWorkflow(task.Name, deployPatterns) {
			agentTier := agentFilter.Labels["tier"]

			// Boost on-demand/n2 agents
			if boost, ok := deployTierBoost[agentTier]; ok {
				score += boost
			}

			// Hold window: spot agents skip deploy tasks for 30s
			// to give on-demand agents time to boot and claim them.
			if agentTier == "spot" || agentTier == "" {
				deployFirstSeenMu.Lock()
				firstSeen, exists := deployFirstSeen[task.ID]
				if !exists {
					deployFirstSeen[task.ID] = time.Now()
					firstSeen = time.Now()
				}
				deployFirstSeenMu.Unlock()

				if time.Since(firstSeen) < deployHoldWindow {
					return false, 0 // reject — waiting for on-demand agent
				}
				// Hold expired — let spot take it as fallback
			}
		}

		return true, score
	}
}

func requiredLabelsMissing(taskLabels, agentLabels map[string]string) bool {
	for label, value := range agentLabels {
		if len(label) > 0 && label[0] == '!' {
			val, ok := taskLabels[label[1:]]
			if !ok || val != value {
				return true
			}
		}
	}
	return false
}

// isDeployWorkflow checks if a workflow name matches any deploy pattern.
func isDeployWorkflow(name string, patterns []string) bool {
	name = strings.ToLower(name)
	for _, p := range patterns {
		if strings.Contains(name, strings.ToLower(p)) {
			return true
		}
	}
	return false
}
