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

package api

import (
	"net/http"
	"sync"
	"time"
)

// webhookDeliveryID extracts a forge-provided, presumed-unique-per-delivery
// identifier from the request, for duplicate-delivery detection (#338).
// GitHub's X-GitHub-Delivery is a UUID that's stable across a manual
// redelivery (UI "Redeliver" / API POST .../attempts reuses the SAME ID —
// it identifies the delivery attempt sequence, not a new event), so
// matching on it catches exactly the "GitHub sent this twice" case without
// false-positiving on a genuinely new event.
//
// Only GitHub is wired up today (the only forge in active Peregrine use).
// Returns "" when no known header is present — callers must treat that as
// "can't dedup this one" and let the request proceed, never as "definitely
// not a duplicate".
func webhookDeliveryID(r *http.Request) string {
	if id := r.Header.Get("X-GitHub-Delivery"); id != "" {
		return id
	}
	return ""
}

// webhookDedupWindow bounds how long a delivery ID is remembered. Long
// enough to catch a redelivery or a near-simultaneous double-delivery
// (both are seconds-to-low-minutes apart in practice); short enough that
// memory never grows unbounded on a busy server. A duplicate delivery ID
// arriving after the window has passed is treated as new — an acceptable
// gap, since the same-commit guard in cancel.go's pipelineNeedsCancel
// (#338) backstops the one harm that actually matters (a duplicate
// cancelling its own in-flight twin) regardless of this cache's state.
const webhookDedupWindow = 5 * time.Minute

// webhookDedup is a small TTL-bounded set of delivery IDs already
// processed. Best-effort and in-memory by design — it doesn't need to
// survive a restart (a genuine redelivery landing right after a restart is
// rare, and the same-commit cancellation guard covers the worst case
// regardless); a DB table would be persistence for a problem that doesn't
// need persisting.
type webhookDedupCache struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func newWebhookDedupCache() *webhookDedupCache {
	return &webhookDedupCache{seen: make(map[string]time.Time)}
}

// check reports whether id was already recorded (via markSeen) within the
// dedup window. Read-only — does NOT record id itself. Empty id is never
// considered a duplicate: an empty string can't distinguish one delivery
// from another, so treating repeats of "" as duplicates would wrongly dedup
// unrelated deliveries from forges/paths with no known delivery-ID header.
func (c *webhookDedupCache) check(id string, now time.Time) bool {
	if id == "" {
		return false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.evictExpired(now)

	expiry, ok := c.seen[id]
	return ok && now.Before(expiry)
}

// markSeen records id as processed, refreshing its expiry. Deliberately
// separate from check(): callers must only mark an ID seen once processing
// has DEFINITELY succeeded (a pipeline was actually created). Marking it on
// receipt instead would make the cache swallow GitHub's own legitimate
// retry-on-5xx redelivery of a delivery that failed transiently (#321's
// ErrTransientForge path exists specifically so that redelivery happens) —
// the exact opposite of what this cache is for.
func (c *webhookDedupCache) markSeen(id string, now time.Time) {
	if id == "" {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.seen[id] = now.Add(webhookDedupWindow)
}

// evictExpired sweeps expired entries. Called with the lock already held.
// Opportunistic (runs on every check) rather than a background goroutine —
// simplest correct option for a cache this small; a busy webhook receiver
// naturally sweeps itself continuously.
func (c *webhookDedupCache) evictExpired(now time.Time) {
	for id, expiry := range c.seen {
		if !now.Before(expiry) {
			delete(c.seen, id)
		}
	}
}

// globalWebhookDedup is the process-wide dedup cache used by PostHook.
// Duplicate short-circuits are counted via the existing webhooksDropped
// metric (reason="duplicate_delivery") rather than a new counter — same
// dashboard, one less metric to add.
var globalWebhookDedup = newWebhookDedupCache()
