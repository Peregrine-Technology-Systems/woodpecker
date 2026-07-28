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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestWebhookDeliveryID(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/api/hook", nil)
	assert.Empty(t, webhookDeliveryID(r), "no header — no ID")

	r.Header.Set("X-GitHub-Delivery", "72d3162e-cc78-11e3-81ab-4c9367dc0958")
	assert.Equal(t, "72d3162e-cc78-11e3-81ab-4c9367dc0958", webhookDeliveryID(r))
}

func TestWebhookDedupCache_EmptyIDNeverDuplicate(t *testing.T) {
	c := newWebhookDedupCache()
	now := time.Unix(1_700_000_000, 0)

	// An empty ID must never register as seen, and must never report as a
	// duplicate — repeats of "" don't distinguish real deliveries from each
	// other, so treating them as dups would wrongly swallow unrelated ones.
	c.markSeen("", now)
	assert.False(t, c.check("", now), "empty id is never a duplicate")
}

func TestWebhookDedupCache_CheckDoesNotRecord(t *testing.T) {
	c := newWebhookDedupCache()
	now := time.Unix(1_700_000_000, 0)

	assert.False(t, c.check("d1", now), "unseen id — not a duplicate")
	// check() alone must not record — only markSeen() does. A delivery that
	// never succeeds (transient error, filtered, etc.) must stay retryable.
	assert.False(t, c.check("d1", now), "check() is read-only — repeating it changes nothing")
}

func TestWebhookDedupCache_MarkSeenThenCheckIsDuplicate(t *testing.T) {
	c := newWebhookDedupCache()
	now := time.Unix(1_700_000_000, 0)

	c.markSeen("d1", now)
	assert.True(t, c.check("d1", now), "marked seen — now a duplicate")
	assert.True(t, c.check("d1", now.Add(webhookDedupWindow-time.Second)), "still within the window")
}

func TestWebhookDedupCache_ExpiresAfterWindow(t *testing.T) {
	c := newWebhookDedupCache()
	now := time.Unix(1_700_000_000, 0)

	c.markSeen("d1", now)
	assert.False(t, c.check("d1", now.Add(webhookDedupWindow+time.Second)), "outside the window — no longer a duplicate")
}

func TestWebhookDedupCache_EvictionSweepsExpiredEntries(t *testing.T) {
	c := newWebhookDedupCache()
	now := time.Unix(1_700_000_000, 0)

	c.markSeen("old", now)
	// A later check (for a different id) triggers the opportunistic sweep;
	// the expired "old" entry must be gone from the underlying map, not just
	// unreachable via check().
	c.check("new", now.Add(webhookDedupWindow+time.Second))

	c.mu.Lock()
	_, stillPresent := c.seen["old"]
	c.mu.Unlock()
	assert.False(t, stillPresent, "expired entry swept from the map, not just logically stale")
}

func TestWebhookDedupCache_IndependentIDsDoNotCollide(t *testing.T) {
	c := newWebhookDedupCache()
	now := time.Unix(1_700_000_000, 0)

	c.markSeen("d1", now)
	assert.False(t, c.check("d2", now), "a different delivery ID is never a duplicate of another")
}

// TestWebhookDedupCache_ConcurrentReadWrite — the mutex must actually
// serialize access under real concurrent traffic (many webhooks can arrive
// at once), not just work in a single-goroutine test. N readers + M writers
// hammering the same small set of IDs under -race.
func TestWebhookDedupCache_ConcurrentReadWrite(t *testing.T) {
	c := newWebhookDedupCache()
	now := time.Now()

	const goroutines = 50
	const opsEach = 200
	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	for g := 0; g < goroutines; g++ {
		id := "id-" + strconv.Itoa(g%10) // deliberate overlap across goroutines

		go func(id string) {
			defer wg.Done()
			for i := 0; i < opsEach; i++ {
				c.markSeen(id, now)
			}
		}(id)

		go func(id string) {
			defer wg.Done()
			for i := 0; i < opsEach; i++ {
				c.check(id, now)
			}
		}(id)
	}

	wg.Wait()

	// Sanity: every id that was ever marked seen must report as seen now.
	for g := 0; g < 10; g++ {
		assert.True(t, c.check(fmt.Sprintf("id-%d", g), now), "id-%d should be seen after concurrent marks", g)
	}
}
