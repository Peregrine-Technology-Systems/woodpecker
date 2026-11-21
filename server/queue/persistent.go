// Copyright 2021 Woodpecker Authors
// Copyright 2018 Drone.IO Inc.
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

import (
	"context"
	"errors"
	"fmt"

	"github.com/rs/zerolog/log"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/store"
	"go.woodpecker-ci.org/woodpecker/v3/server/store/types"
)

// WithTaskStore returns a queue that is backed by the TaskStore. This
// ensures the task Queue can be restored when the system starts.
// Zombie tasks — tasks whose parent pipeline is in a terminal state — are
// evicted from the store and not loaded into the in-memory queue (#154).
// Tasks an agent was running are restored as running on that agent (#400).
func WithTaskStore(ctx context.Context, q Queue, s store.Store) Queue {
	tasks, _ := s.TaskList()
	plan := planRestore(s, tasks)

	evicted := 0
	for _, id := range plan.evict {
		if deleteErr := s.TaskDelete(id); deleteErr != nil {
			log.Error().Err(deleteErr).Msgf("queue restore: failed to delete zombie task %s", id)
			continue
		}
		log.Info().Msgf("queue restore: evicted finished task %s", id)
		evicted++
	}
	if evicted > 0 {
		log.Info().Msgf("queue restore: evicted %d zombie tasks from terminal pipelines or workflows (#154, #400)", evicted)
	}

	if err := q.PushAtOnce(ctx, plan.pending); err != nil {
		log.Error().Err(err).Msg("PushAtOnce failed")
	}
	if len(plan.running) > 0 {
		if adopter, ok := q.(runningAdopter); ok {
			adopter.adoptRunning(plan.running)
			log.Info().Msgf("queue restore: restored %d running tasks on their agents (#400)", len(plan.running))
		} else {
			log.Error().Msgf("queue restore: %d running tasks not restored — queue cannot adopt running work", len(plan.running))
		}
	}
	return &persistentQueue{q, s}
}

// isTerminalPipeline returns true when a pipeline has reached a final state
// and will never dispatch further tasks. Tasks from terminal pipelines are
// zombies — they will never be picked up and should be evicted on startup.
func isTerminalPipeline(status model.StatusValue) bool {
	switch status {
	case model.StatusSuccess, model.StatusFailure, model.StatusKilled,
		model.StatusError, model.StatusDeclined, model.StatusSkipped,
		model.StatusSuperseded, model.StatusCanceled:
		return true
	default:
		return false
	}
}

type persistentQueue struct {
	Queue
	store store.Store
}

// PushAtOnce pushes multiple tasks to the tail of this queue.
func (q *persistentQueue) PushAtOnce(c context.Context, tasks []*model.Task) error {
	// TODO: invent store.NewSession who return context including a session and make TaskInsert & TaskDelete use it
	for _, task := range tasks {
		if err := q.store.TaskInsert(task); err != nil {
			return err
		}
	}
	err := q.Queue.PushAtOnce(c, tasks)
	if err != nil {
		for _, task := range tasks {
			if err := q.store.TaskDelete(task.ID); err != nil {
				return err
			}
		}
	}
	return err
}

// Poll retrieves and removes a task head of this queue. The stored row is
// kept until the work is finished (Done/Error), so a restart can restore the
// work agents are running instead of forgetting it (#400).
func (q *persistentQueue) Poll(c context.Context, agentID int64, f FilterFn) (*model.Task, error) {
	return q.Queue.Poll(c, agentID, f)
}

// Done signals the task is finished and removes it from the store (#400).
func (q *persistentQueue) Done(c context.Context, id string, exitStatus model.StatusValue) error {
	if err := q.Queue.Done(c, id, exitStatus); err != nil {
		return err
	}
	return q.forget(id)
}

// forget removes a finished task's row; a row already gone is not an error.
func (q *persistentQueue) forget(id string) error {
	if deleteErr := q.store.TaskDelete(id); deleteErr != nil {
		if !errors.Is(deleteErr, types.ErrRecordNotExist) {
			return deleteErr
		}
		log.Debug().Msgf("task %s already removed from store", id)
	}
	return nil
}

// Error signals the task is done with an error.
func (q *persistentQueue) Error(c context.Context, id string, err error) error {
	if err := q.Queue.Error(c, id, err); err != nil {
		return err
	}
	return q.forget(id)
}

// ErrorAtOnce signals multiple tasks are done and complete with an error.
// If still pending they will just get removed from the queue.
func (q *persistentQueue) ErrorAtOnce(c context.Context, ids []string, err error) error {
	if err := q.Queue.ErrorAtOnce(c, ids, err); err != nil {
		return err
	}

	var errs []error
	for _, id := range ids {
		if deleteErr := q.store.TaskDelete(id); deleteErr != nil && !errors.Is(deleteErr, types.ErrRecordNotExist) {
			errs = append(errs, fmt.Errorf("task id [%s]: %w", id, deleteErr))
		}
	}

	if len(errs) != 0 {
		return fmt.Errorf("failed to delete tasks from persistent store: %w", errors.Join(errs...))
	}
	return nil
}

// Requeue rewrites the task's stored row before delegating to the in-memory
// queue, so it survives a restart as waiting work (#225). The row normally
// still exists, because it lives until Done/Error (#400); it is replaced
// because the re-queued task no longer belongs to its old agent.
func (q *persistentQueue) Requeue(c context.Context, task *model.Task) error {
	if err := q.forget(task.ID); err != nil {
		return err
	}
	if err := q.store.TaskInsert(task); err != nil {
		return err
	}
	return q.Queue.Requeue(c, task)
}

// UpdatePriority mutates both the in-memory queue position and the
// persisted DB row so the priority survives a server restart. The
// in-memory mutation must succeed first — if it fails (task not pending)
// we return ErrNotFound without touching the DB. (#47)
func (q *persistentQueue) UpdatePriority(taskID string, newPriority int64) (int64, error) {
	old, err := q.Queue.UpdatePriority(taskID, newPriority)
	if err != nil {
		return 0, err
	}
	if dbErr := q.store.UpdateTaskPriority(taskID, newPriority); dbErr != nil {
		// In-memory updated but DB didn't — log loudly. Next restart will
		// re-seed from DB and the in-memory mutation is lost. Acceptable
		// at this scale (low-thousands of tasks; restart re-syncs).
		log.Error().Err(dbErr).Str("task_id", taskID).
			Int64("old_priority", old).Int64("new_priority", newPriority).
			Msg("priority: in-memory updated but DB update failed — persistence drift until next restart")
		return old, dbErr
	}
	return old, nil
}
