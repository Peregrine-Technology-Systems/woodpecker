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

package rpc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #429: a read pump that exits must only clean up its own connection. When a
// redial installed a new connection before the old pump's cleanup ran, the old
// pump closed the new one, whose pump then closed the next: the agent
// reconnected forever, each session dying within a second (1006 on the
// server, #425).

// holdingServer accepts connections and keeps each open, counting the ones
// still open.
func holdingServer(t *testing.T) (open *atomic.Int64, srv *httptest.Server) {
	t.Helper()
	open = new(atomic.Int64)
	hold := holdUntilCleanup(t)
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		open.Add(1)
		defer open.Add(-1)
		go func() { <-hold; conn.Close() }()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return open, srv
}

func TestWSClient_ARedialSurvivesTheOldReadPump(t *testing.T) {
	open, srv := holdingServer(t)
	c := NewWSClient(context.Background(), srv.URL[7:], "test-secret", "agent-1", false).(*WSClient)
	ctx, cancel := context.WithTimeout(context.Background(), wsTestDeadline)
	defer cancel()
	require.NoError(t, c.connect(ctx))
	require.Eventually(t, func() bool { return open.Load() == 1 }, wsTestDeadline, 5*time.Millisecond)

	// A redial while the first connection's pump is still running: connect
	// closes the old socket and installs a new one; the old pump then exits.
	require.NoError(t, c.connect(ctx))

	// The first connection is gone and the second must stay up.
	require.Eventually(t, func() bool { return open.Load() == 1 }, wsTestDeadline, 5*time.Millisecond)
	time.Sleep(200 * time.Millisecond) // well past the old pump's exit
	c.mu.Lock()
	alive := c.conn != nil
	c.mu.Unlock()
	assert.True(t, alive, "the old read pump cleared the new connection")
	assert.Equal(t, int64(1), open.Load(), "the old read pump closed the new connection")
}
