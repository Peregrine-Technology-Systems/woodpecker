// Copyright 2026 Peregrine Technology Systems
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

package datastore

// writeQueue serializes all SQLite write operations through a single goroutine,
// eliminating SQLITE_BUSY "database table is locked" errors under concurrent
// write load. Without serialization, concurrent HTTP handlers (pipeline status
// updates, webhook ingestion, agent registrations) race on the SQLite writer
// lock — even with busy_timeout set, heavy bursts produce lock errors that
// surface as server freezes and cascading healthcheck failures. (#55)
//
// Architecture: callers send a writeOp (a closure over the write operation and
// a result channel) to the bounded ops channel. The single drain goroutine
// executes each op serially and sends the result back. Callers block until
// their op completes or the context is cancelled.
//
// Reads bypass the queue entirely — SQLite WAL mode allows concurrent readers
// alongside the single writer, so reads use the normal xorm engine pool.
//
// Non-SQLite drivers: wq is nil and serialize() calls the fn directly, adding
// zero overhead and requiring no driver-specific branching in callers.

const writeQueueDepth = 256

type writeOp struct {
	fn     func() error
	result chan<- error
}

type writeQueue struct {
	ops chan writeOp
}

// newWriteQueue creates a write queue with the given buffer depth and starts
// the drain goroutine. Call close() when the store is shut down.
func newWriteQueue(depth int) *writeQueue {
	wq := &writeQueue{ops: make(chan writeOp, depth)}
	go wq.drain()
	return wq
}

func (wq *writeQueue) drain() {
	for op := range wq.ops {
		op.result <- op.fn()
	}
}

// close drains any pending ops and stops the drain goroutine. Ops already in
// the queue complete normally; callers blocked on wq.ops <- will receive a
// closed-channel panic (callers should not send after close — the store's
// Close() method is the only call site).
func (wq *writeQueue) close() {
	close(wq.ops)
}

// serialize runs fn on the drain goroutine, blocking until it completes.
// If wq is nil (non-SQLite driver), fn is called directly on the caller's
// goroutine — semantics are identical, overhead is zero.
func (wq *writeQueue) serialize(fn func() error) error {
	if wq == nil {
		return fn()
	}
	result := make(chan error, 1)
	wq.ops <- writeOp{fn: fn, result: result}
	return <-result
}
