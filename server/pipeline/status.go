// Copyright 2026 Woodpecker Authors
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

package pipeline

import "go.woodpecker-ci.org/woodpecker/v3/server/model"

// list of statuses by their priority. Most important is on top.
var statusPriorityOrder = []model.StatusValue{
	// blocked, declined and created cannot appear in the
	// same workflow/pipeline at the same time
	model.StatusDeclined,
	model.StatusBlocked,
	model.StatusCreated,

	// errors have highest priority.
	model.StatusError,

	// skipped and killed/superseded cannot appear together with running/pending.
	model.StatusKilled,
	model.StatusSuperseded,
	model.StatusCanceled,

	// running states
	model.StatusRunning,
	model.StatusPending,

	// finished states
	model.StatusFailure,
	model.StatusPartial, // mixed success+killed; less severe than failure, more than pure success (woodpecker-server#28)
	model.StatusSuccess,

	// skipped due to status condition
	model.StatusSkipped,
}

var priorityMap map[model.StatusValue]int = buildPriorityMap()

func buildPriorityMap() map[model.StatusValue]int {
	m := map[model.StatusValue]int{}
	for i, s := range statusPriorityOrder {
		m[s] = i
	}
	return m
}

func MergeStatusValues(s, t model.StatusValue) model.StatusValue {
	// both are skipped due to cancellation -> canceled
	if s == model.StatusCanceled && t == model.StatusCanceled {
		return model.StatusCanceled
	}
	// if only one was skipped -> use killed as the workflow/pipeline was running once already
	if s == model.StatusCanceled {
		s = model.StatusKilled
	}
	if t == model.StatusCanceled {
		t = model.StatusKilled
	}
	// superseded behaves like killed for merge purposes
	if s == model.StatusSuperseded {
		s = model.StatusKilled
	}
	if t == model.StatusSuperseded {
		t = model.StatusKilled
	}

	// Partial: at least one workflow succeeded AND at least one was killed.
	// Distinguishes "deploy succeeded but version-bump killed" from "everything killed".
	// (woodpecker-server#28)
	sSuccess, tSuccess := s == model.StatusSuccess, t == model.StatusSuccess
	sKilled, tKilled := s == model.StatusKilled, t == model.StatusKilled
	sPartial, tPartial := s == model.StatusPartial, t == model.StatusPartial
	if (sSuccess && tKilled) || (tSuccess && sKilled) {
		return model.StatusPartial
	}
	if sPartial && tPartial {
		return model.StatusPartial
	}
	if sPartial || tPartial {
		other := t
		if tPartial {
			other = s
		}
		// Partial absorbs further successes and further killed-likes.
		if other == model.StatusSuccess || other == model.StatusKilled {
			return model.StatusPartial
		}
		// Otherwise the non-partial side is heavier (running, pending, error, failure,
		// blocked, declined, created) and wins.
		return other
	}

	return statusPriorityOrder[min(priorityMap[s], priorityMap[t])]
}
