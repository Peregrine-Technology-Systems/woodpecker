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

package queue

// Holder returns the agent holding a running task (#418). A task that is
// pending, waiting on dependencies, finished or unknown has no holder and
// returns ErrNotFound.
func (q *fifo) Holder(taskID string) (int64, error) {
	q.Lock()
	defer q.Unlock()

	state, ok := q.running[taskID]
	if !ok {
		return 0, ErrNotFound
	}
	return state.item.AgentID, nil
}
