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

package queue

import (
	"container/list"
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/shared/constant"
)

type entry struct {
	item     *model.Task
	done     chan bool
	error    error
	deadline time.Time
}

type worker struct {
	agentID int64
	filter  FilterFn
	channel chan *model.Task
	stop    context.CancelCauseFunc
}

type fifo struct {
	sync.Mutex

	ctx           context.Context
	workers       map[*worker]struct{}
	running       map[string]*entry
	pending       *list.List
	waitingOnDeps *list.List
	extension     time.Duration
	paused        bool
	dispatchHook  DispatchFunc

	// #243/#246: owner-liveness reclaim. agentKnownDead reports whether a running
	// task's owning agent has been POSITIVELY observed as disconnected (its WS
	// reconnect grace expired) — it fails SAFE, returning false for any agent
	// whose liveness was never tracked (e.g. gRPC/local-backend agents), so they
	// are never reclaimed early (#246). reclaimOrphan releases the stranded
	// task(s) for an agent found dead. reclaimFiredAt throttles re-firing reclaim
	// for the same agent across consecutive ticks while a prior reclaim (store
	// I/O) is still settling.
	agentKnownDead func(agentID int64) bool
	reclaimOrphan  func(agentID int64)
	reclaimFiredAt map[int64]time.Time

	// #248: transport-agnostic, OBSERVE-ONLY liveness. agentStale reports whether
	// a running task's owning agent has not refreshed LastContact within the
	// staleness window. Unlike agentKnownDead (WS positive-disconnect only), this
	// also covers gRPC/local-backend agents, so it makes the previously-invisible
	// "running task stranded on a dead agent" manifestation (B) measurable. It
	// feeds ONLY the running_owner_stale gauge — it NEVER triggers a reclaim
	// (reclaiming on mere LastContact aging would re-introduce the t+0s kill #246
	// fixed). Fails safe: nil, or returning false, simply leaves the gauge at 0.
	agentStale func(agentID int64) bool
}

// processTimeInterval is the time till the queue rearranges things,
// as the agent pull in 10 milliseconds we should also give them work asap.
const processTimeInterval = 100 * time.Millisecond

// #243 owner-liveness reclaim tuning. Vars (not consts) so tests can shrink them.
var (
	// orphanAgeThreshold is how long a dispatchable task may sit in q.pending
	// before it is reported as pending_dispatchable. Set above the normal
	// pick-up latency (sub-second) so healthy tasks never register.
	orphanAgeThreshold = 120 * time.Second

	// reclaimRefireInterval bounds how often the queue re-fires reclaim for one
	// disconnected agent — one reclaim covers all of that agent's stranded
	// tasks, and ReleaseAgentTasks may take longer than a tick to settle.
	reclaimRefireInterval = 5 * time.Second
)

// NewMemoryQueue returns a new fifo queue.
func NewMemoryQueue(ctx context.Context) Queue {
	q := &fifo{
		ctx:            ctx,
		workers:        map[*worker]struct{}{},
		running:        map[string]*entry{},
		pending:        list.New(),
		waitingOnDeps:  list.New(),
		extension:      constant.TaskTimeout,
		paused:         false,
		reclaimFiredAt: map[int64]time.Time{},
	}
	go q.process()
	return q
}

// PushAtOnce pushes multiple tasks into this queue at the position implied
// by Priority (higher dispatches first). FIFO within ties is preserved by
// inserting at the back of any equal-priority block. Default priority 0
// means new tasks land at the back of the queue, matching pre-#46 behavior.
func (q *fifo) PushAtOnce(_ context.Context, tasks []*model.Task) error {
	q.Lock()
	for _, task := range tasks {
		q.insertByPriority(task)
	}
	q.Unlock()
	return nil
}

// insertByPriority inserts task into q.pending so the list stays sorted by
// Priority DESC. Within a priority bucket the new task lands at the back —
// `existing.Priority < task.Priority` is strict, so equal-priority entries
// are walked past, preserving FIFO order within ties (#46).
//
// Caller must hold q.Mutex.
func (q *fifo) insertByPriority(task *model.Task) {
	for e := q.pending.Front(); e != nil; e = e.Next() {
		existing, _ := e.Value.(*model.Task)
		if existing.Priority < task.Priority {
			q.pending.InsertBefore(task, e)
			return
		}
	}
	q.pending.PushBack(task)
}

// Poll retrieves and removes a task head of this queue.
func (q *fifo) Poll(c context.Context, agentID int64, filter FilterFn) (*model.Task, error) {
	q.Lock()
	ctx, stop := context.WithCancelCause(c)

	w := &worker{
		agentID: agentID,
		channel: make(chan *model.Task, 1),
		filter:  filter,
		stop:    stop,
	}
	q.workers[w] = struct{}{}
	q.Unlock()

	for {
		select {
		case <-ctx.Done():
			q.Lock()
			delete(q.workers, w)
			q.Unlock()
			return nil, ctx.Err()
		case t := <-w.channel:
			return t, nil
		}
	}
}

// Done signals the task is complete.
func (q *fifo) Done(_ context.Context, id string, exitStatus model.StatusValue) error {
	return q.finished([]string{id}, exitStatus, nil)
}

// Error signals the task is done with an error.
func (q *fifo) Error(_ context.Context, id string, err error) error {
	return q.finished([]string{id}, model.StatusFailure, err)
}

// ErrorAtOnce signals multiple tasks are done and complete with an error.
// If still pending they will just get removed from the queue.
func (q *fifo) ErrorAtOnce(_ context.Context, ids []string, err error) error {
	if errors.Is(err, ErrCancel) {
		return q.finished(ids, model.StatusKilled, err)
	}
	return q.finished(ids, model.StatusFailure, err)
}

// locks the queue itself!
func (q *fifo) finished(ids []string, exitStatus model.StatusValue, err error) error {
	q.Lock()
	defer q.Unlock()

	// it's an external error so we wrap it
	err = NewErrExternal(err)

	var errs []error
	// we first process the tasks itself
	for _, id := range ids {
		if taskEntry, ok := q.running[id]; ok {
			taskEntry.error = err
			close(taskEntry.done)
			delete(q.running, id)
		} else {
			errs = append(errs, q.removeFromPendingAndWaiting(id))
		}
	}

	// next we aim for there dependencies
	// we do this because in our ids list there could be tasks and its dependencies
	// so not to mess things up
	for _, id := range ids {
		q.updateDepStatusInQueue(id, exitStatus)
	}

	return errors.Join(errs...)
}

// Wait waits until the item is done executing.
// Also signals via error ErrCancel if workflow got canceled.
func (q *fifo) Wait(ctx context.Context, taskID string) error {
	q.Lock()
	state := q.running[taskID]
	q.Unlock()
	if state != nil {
		select {
		case <-ctx.Done():
		case <-state.done:
			// only return queue errors and no workflow errors
			if !errors.Is(state.error, new(ErrExternal)) {
				return state.error
			}
		}
	}
	return nil
}

// Extend extends the task execution deadline.
func (q *fifo) Extend(_ context.Context, agentID int64, taskID string) error {
	q.Lock()
	defer q.Unlock()

	state, ok := q.running[taskID]
	if ok {
		if state.item.AgentID != agentID {
			return ErrAgentMissMatch
		}

		state.deadline = time.Now().Add(q.extension)
		return nil
	}
	return ErrNotFound
}

// Info returns internal queue information.
func (q *fifo) Info(_ context.Context) InfoT {
	q.Lock()
	stats := InfoT{}
	stats.Stats.Workers = len(q.workers)
	stats.Stats.Pending = q.pending.Len()
	stats.Stats.WaitingOnDeps = q.waitingOnDeps.Len()
	stats.Stats.Running = len(q.running)

	for element := q.pending.Front(); element != nil; element = element.Next() {
		task, _ := element.Value.(*model.Task)
		stats.Pending = append(stats.Pending, task)
	}
	for element := q.waitingOnDeps.Front(); element != nil; element = element.Next() {
		task, _ := element.Value.(*model.Task)
		stats.WaitingOnDeps = append(stats.WaitingOnDeps, task)
	}
	for _, entry := range q.running {
		stats.Running = append(stats.Running, entry.item)
	}
	stats.Paused = q.paused

	q.Unlock()
	return stats
}

// Pause stops the queue from handing out new work items in Poll.
func (q *fifo) Pause() {
	q.Lock()
	q.paused = true
	q.Unlock()
}

// Resume starts the queue again.
func (q *fifo) Resume() {
	q.Lock()
	q.paused = false
	q.Unlock()
}

// KickAgentWorkers kicks all workers for a given agent.
func (q *fifo) KickAgentWorkers(agentID int64) {
	q.Lock()
	defer q.Unlock()

	for worker := range q.workers {
		if worker.agentID == agentID {
			worker.stop(ErrWorkerKicked)
			delete(q.workers, worker)
		}
	}
}

// SetDispatchHook sets a function called for each pending task before
// worker assignment. If it returns handled=true, the task moves to running.
func (q *fifo) SetDispatchHook(fn DispatchFunc) {
	q.Lock()
	defer q.Unlock()
	q.dispatchHook = fn
}

// SetAgentReclaimFn injects the fail-safe known-dead oracle + reclaim callback
// (#243, hardened #246).
func (q *fifo) SetAgentReclaimFn(knownDead func(int64) bool, reclaim func(int64)) {
	q.Lock()
	defer q.Unlock()
	q.agentKnownDead = knownDead
	q.reclaimOrphan = reclaim
}

// SetAgentStaleFn injects the observe-only LastContact-aged liveness oracle
// (#248). It feeds the running_owner_stale gauge only — never a reclaim. May be
// nil (the gauge then stays at zero).
func (q *fifo) SetAgentStaleFn(stale func(int64) bool) {
	q.Lock()
	defer q.Unlock()
	q.agentStale = stale
}

// sampleOrphans updates the #243 orphaned-workflow gauges and returns the set
// of agent IDs that own a running task but are no longer connected and are due
// for a reclaim re-fire (throttled by reclaimRefireInterval). Caller must hold
// q.Mutex. The actual reclaim runs after the lock is released — ReleaseAgentTasks
// re-enters the queue (q.Info, q.Requeue), so firing it under the lock deadlocks.
func (q *fifo) sampleOrphans(now time.Time) []int64 {
	// pending_dispatchable: tasks that could run now but have aged out.
	pendingDispatchable := 0
	for e := q.pending.Front(); e != nil; e = e.Next() {
		task, _ := e.Value.(*model.Task)
		if task.ShouldRun() && task.Created > 0 && now.Unix()-task.Created > int64(orphanAgeThreshold.Seconds()) {
			pendingDispatchable++
		}
	}
	setOrphanedWorkflows("pending_dispatchable", float64(pendingDispatchable))

	// waiting_on_deps_aged (#245): tasks parked in q.waitingOnDeps past the same
	// threshold. The #243 / infra#5265 incident could not tell — 100 min after
	// the fact — whether the stuck workflow was pending_dispatchable (deps met,
	// no matching worker) or stuck in waitingOnDeps (deps looked unsatisfied).
	// Surfacing both buckets makes the next recurrence self-diagnosing: it
	// answers "which queue was the orphan in?" directly from metrics instead of
	// by-eye guessing. Observe-only — no re-dispatch is driven from this.
	waitingAged := 0
	for e := q.waitingOnDeps.Front(); e != nil; e = e.Next() {
		task, _ := e.Value.(*model.Task)
		if task.Created > 0 && now.Unix()-task.Created > int64(orphanAgeThreshold.Seconds()) {
			waitingAged++
		}
	}
	setOrphanedWorkflows("waiting_on_deps_aged", float64(waitingAged))

	// Single pass over q.running computes two independent gauges plus the reclaim
	// dispatch list:
	//   - running_dead_owner: owner POSITIVELY known gone via the WS reconnect-
	//     grace path. Drives the reclaim (released within a tick, not at the
	//     TaskTimeout lease). Fails safe (#246) — an agent never tracked through
	//     the WS path (e.g. a gRPC/local-backend agent) is NOT known-dead, so it
	//     is left alone rather than killed at t+0s.
	//   - running_owner_stale (#248): owner's LastContact aged past the staleness
	//     window. TRANSPORT-AGNOSTIC and OBSERVE-ONLY — it covers gRPC/local
	//     agents the WS registry never sees (manifestation B), but drives NO
	//     reclaim. Counting it independently is the measure-first step before any
	//     transport-agnostic reclaim is built.
	deadOwnerRunning := 0
	staleOwnerRunning := 0
	var due []int64
	seen := make(map[int64]struct{})
	for _, e := range q.running {
		agentID := e.item.AgentID
		if agentID <= 0 {
			continue // unclaimed (dispatch hook)
		}
		if q.agentStale != nil && q.agentStale(agentID) {
			staleOwnerRunning++
		}
		if q.agentKnownDead == nil || !q.agentKnownDead(agentID) {
			continue // owner not WS-known-dead — the reclaim path does not act
		}
		deadOwnerRunning++
		if _, ok := seen[agentID]; ok {
			continue
		}
		seen[agentID] = struct{}{}
		if last := q.reclaimFiredAt[agentID]; now.Sub(last) >= reclaimRefireInterval {
			q.reclaimFiredAt[agentID] = now
			due = append(due, agentID)
		}
	}
	// drop throttle entries for agents no longer owning running tasks
	for agentID := range q.reclaimFiredAt {
		if _, ok := seen[agentID]; !ok {
			delete(q.reclaimFiredAt, agentID)
		}
	}
	setOrphanedWorkflows("running_dead_owner", float64(deadOwnerRunning))
	setOrphanedWorkflows("running_owner_stale", float64(staleOwnerRunning))
	return due
}

// Requeue atomically moves a running task back to pending without propagating
// dep-status updates to dependent tasks (#225). If the task is no longer in
// q.running (e.g. resubmitExpiredPipelines already moved it), the provided
// task is inserted into pending directly to avoid losing it.
func (q *fifo) Requeue(_ context.Context, task *model.Task) error {
	q.Lock()
	defer q.Unlock()

	if state, ok := q.running[task.ID]; ok {
		q.insertByPriority(state.item)
		delete(q.running, task.ID)
		close(state.done)
		return nil
	}
	// Not in running — insert directly (resubmit already moved it, or it was
	// never recorded as running due to a race).
	q.insertByPriority(task)
	return nil
}

// UpdatePriority finds taskID in q.pending or q.waitingOnDeps, mutates its
// Priority field, and (if it was pending) re-inserts at the correct
// position so dispatch reflects the new priority. Returns ErrNotFound if
// the task is currently running, completed, or absent. (#47)
func (q *fifo) UpdatePriority(taskID string, newPriority int64) (int64, error) {
	q.Lock()
	defer q.Unlock()

	for e := q.pending.Front(); e != nil; e = e.Next() {
		task, _ := e.Value.(*model.Task)
		if task.ID == taskID {
			old := task.Priority
			task.Priority = newPriority
			q.pending.Remove(e)
			q.insertByPriority(task)
			return old, nil
		}
	}

	for e := q.waitingOnDeps.Front(); e != nil; e = e.Next() {
		task, _ := e.Value.(*model.Task)
		if task.ID == taskID {
			old := task.Priority
			task.Priority = newPriority
			// No re-insert here — when filterWaiting moves it back to
			// pending it'll route through insertByPriority with the new
			// value.
			return old, nil
		}
	}

	return 0, ErrNotFound
}

// helper function that loops through the queue and attempts to
// match the item to a single subscriber until context got cancel.
func (q *fifo) process() {
	for {
		select {
		case <-time.After(processTimeInterval):
		case <-q.ctx.Done():
			return
		}

		q.Lock()
		if q.paused {
			q.Unlock()
			continue
		}

		q.resubmitExpiredPipelines()
		q.filterWaiting()

		// External dispatch: offer pending tasks to the hook before agent assignment
		if q.dispatchHook != nil {
			for element := q.pending.Front(); element != nil; {
				task, _ := element.Value.(*model.Task)
				next := element.Next()
				if handled, _ := q.dispatchHook(q.ctx, task); handled {
					q.pending.Remove(element)
					q.running[task.ID] = &entry{
						item:     task,
						done:     make(chan bool),
						deadline: time.Now().Add(q.extension),
					}
					log.Debug().Str("task", task.ID).Msg("queue: task claimed by dispatch hook")
				}
				element = next
			}
		}

		for pending, worker := q.assignToWorker(); pending != nil && worker != nil; pending, worker = q.assignToWorker() {
			task, _ := pending.Value.(*model.Task)
			task.AgentID = worker.agentID
			delete(q.workers, worker)
			q.pending.Remove(pending)
			q.running[task.ID] = &entry{
				item:     task,
				done:     make(chan bool),
				deadline: time.Now().Add(q.extension),
			}
			worker.channel <- task
		}

		// #243: sample orphan gauges and collect dead-owner agents while still
		// holding the lock; fire the reclaim callback only after unlocking
		// (ReleaseAgentTasks re-enters the queue and would deadlock otherwise).
		orphanAgents := q.sampleOrphans(time.Now())
		reclaim := q.reclaimOrphan
		q.Unlock()

		for _, agentID := range orphanAgents {
			recordOrphanReclaim()
			if reclaim != nil {
				go reclaim(agentID)
			}
		}
	}
}

func (q *fifo) filterWaiting() {
	// resubmits all waiting tasks to pending, deps may have cleared.
	// Re-insertion is priority-aware (#46) so a high-priority task whose
	// deps just cleared dispatches ahead of low-priority pending tasks.
	for element := q.waitingOnDeps.Front(); element != nil; element = element.Next() {
		task, _ := element.Value.(*model.Task)
		q.insertByPriority(task)
	}

	// rebuild waitingDeps
	q.waitingOnDeps = list.New()
	var filtered []*list.Element
	for element := q.pending.Front(); element != nil; element = element.Next() {
		task, _ := element.Value.(*model.Task)
		if q.depsInQueue(task) {
			log.Debug().Str("task_id", task.ID).Strs("run_on", task.RunOn).Any("dep_status", task.DepStatus).Msg("filterWaiting: task skipped — unmet deps (#162)")
			q.waitingOnDeps.PushBack(task)
			filtered = append(filtered, element)
		}
	}

	// filter waiting tasks
	for _, f := range filtered {
		q.pending.Remove(f)
	}
}

func (q *fifo) assignToWorker() (*list.Element, *worker) {
	var bestWorker *worker
	var bestScore int

	for element := q.pending.Front(); element != nil; element = element.Next() {
		task, _ := element.Value.(*model.Task)
		log.Debug().Msgf("queue: trying to assign task: %v with deps %v", task.ID, task.Dependencies)

		for worker := range q.workers {
			matched, score := worker.filter(task)
			if matched && score > bestScore {
				bestWorker = worker
				bestScore = score
			}
		}
		if bestWorker != nil {
			log.Debug().Msgf("queue: assigned task: %v with deps %v to worker with score %d", task.ID, task.Dependencies, bestScore)
			return element, bestWorker
		}
	}

	return nil, nil
}

func (q *fifo) resubmitExpiredPipelines() {
	for taskID, taskState := range q.running {
		if time.Now().After(taskState.deadline) {
			log.Info().Msgf("queue: resubmitting expired task %s", taskID)
			taskState.error = ErrTaskExpired
			// Re-queue at the back of the task's own priority bucket
			// (#46) — pre-#46 PushFront would let a priority-0 expired
			// task jump ahead of priority-5 pending tasks.
			q.insertByPriority(taskState.item)
			delete(q.running, taskID)
			close(taskState.done)
		}
	}
}

func (q *fifo) depsInQueue(task *model.Task) bool {
	for element := q.pending.Front(); element != nil; element = element.Next() {
		possibleDep, ok := element.Value.(*model.Task)
		log.Debug().Msgf("queue: pending right now: %v", possibleDep.ID)
		for _, dep := range task.Dependencies {
			if ok && possibleDep.ID == dep {
				return true
			}
		}
	}
	for possibleDepID := range q.running {
		log.Debug().Msgf("queue: running right now: %v", possibleDepID)
		if slices.Contains(task.Dependencies, possibleDepID) {
			return true
		}
	}
	return false
}

// expects the q to be currently owned e.g. locked by caller!
func (q *fifo) updateDepStatusInQueue(taskID string, status model.StatusValue) {
	for element := q.pending.Front(); element != nil; element = element.Next() {
		pending, _ := element.Value.(*model.Task)
		for _, dep := range pending.Dependencies {
			if taskID == dep {
				pending.DepStatus[dep] = status
			}
		}
	}

	for _, running := range q.running {
		for _, dep := range running.item.Dependencies {
			if taskID == dep {
				running.item.DepStatus[dep] = status
			}
		}
	}

	for element := q.waitingOnDeps.Front(); element != nil; element = element.Next() {
		waiting, _ := element.Value.(*model.Task)
		for _, dep := range waiting.Dependencies {
			if taskID == dep {
				waiting.DepStatus[dep] = status
				// #192: a waiting task whose dep just transitioned to
				// terminal failure will never dispatch successfully —
				// surface that as a dispatch failure even though the
				// task itself stays in the queue until ShouldRun
				// re-evaluates and skips/cancels it.
				if isTerminalFailure(status) {
					recordDispatchFailure("dependency_unsatisfied_terminal")
				}
			}
		}
	}
}

// isTerminalFailure reports whether a workflow status guarantees that
// dependents will never dispatch (regardless of run_on / when settings).
// Mirrors the upstream model.StatusValue terminal-failure set; kept
// small + local so the queue package doesn't reach across into pipeline
// status logic.
func isTerminalFailure(s model.StatusValue) bool {
	switch s {
	case model.StatusFailure, model.StatusError, model.StatusKilled, model.StatusCanceled:
		return true
	default:
		return false
	}
}

// expects the q to be currently owned e.g. locked by caller!
func (q *fifo) removeFromPendingAndWaiting(taskID string) error {
	log.Debug().Msgf("queue: trying to remove %s", taskID)

	// we assume pending first
	for element := q.pending.Front(); element != nil; element = element.Next() {
		task, _ := element.Value.(*model.Task)
		if task.ID == taskID {
			log.Debug().Msgf("queue: %s is removed from pending", taskID)
			_ = q.pending.Remove(element)
			// #192: task was in pending — never dispatched to a worker.
			recordDispatchFailure("cancelled_before_dispatch")
			return nil
		}
	}

	// well looks like it's waiting
	for element := q.waitingOnDeps.Front(); element != nil; element = element.Next() {
		task, _ := element.Value.(*model.Task)
		if task.ID == taskID {
			log.Debug().Msgf("queue: %s is removed from waitingOnDeps", taskID)
			_ = q.waitingOnDeps.Remove(element)
			// #192: task was waiting for deps and got pulled before they
			// cleared — never dispatched.
			recordDispatchFailure("cancelled_before_dispatch")
			return nil
		}
	}

	// well it could not be found
	return ErrNotFound
}
