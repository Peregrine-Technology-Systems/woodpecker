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

package api

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	store_mocks "go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
	"go.woodpecker-ci.org/woodpecker/v3/server/store/types"
)

// TestIsAgentLiveForReconcile covers the #394 reconcile-hold oracle. Its
// polarity is the reverse of IsAgentLastContactStale's: it gates a KILL, so an
// unknown answer must read as live (hold), and only positive evidence that the
// agent is gone (no row, contact aged out, never contacted) lets the kill run.
func TestIsAgentLiveForReconcile(t *testing.T) {
	orig := AgentStaleThreshold
	AgentStaleThreshold = 90 * time.Second
	t.Cleanup(func() { AgentStaleThreshold = orig })

	t.Run("non-positive id is not a holder", func(t *testing.T) {
		assert.False(t, IsAgentLiveForReconcile(0, store_mocks.NewMockStore(t)))
	})

	t.Run("nil store cannot rule the agent out, so it holds", func(t *testing.T) {
		assert.True(t, IsAgentLiveForReconcile(5, nil))
	})

	t.Run("deleted agent row is gone", func(t *testing.T) {
		s := store_mocks.NewMockStore(t)
		s.On("AgentFind", int64(5)).Return((*model.Agent)(nil), types.ErrRecordNotExist)
		assert.False(t, IsAgentLiveForReconcile(5, s))
	})

	t.Run("other store error holds", func(t *testing.T) {
		s := store_mocks.NewMockStore(t)
		s.On("AgentFind", int64(5)).Return((*model.Agent)(nil), errors.New("db down"))
		assert.True(t, IsAgentLiveForReconcile(5, s))
	})

	t.Run("nil agent without an error holds", func(t *testing.T) {
		s := store_mocks.NewMockStore(t)
		s.On("AgentFind", int64(5)).Return((*model.Agent)(nil), nil)
		assert.True(t, IsAgentLiveForReconcile(5, s))
	})

	t.Run("never-contacted agent is not live", func(t *testing.T) {
		s := store_mocks.NewMockStore(t)
		s.On("AgentFind", int64(5)).Return(&model.Agent{ID: 5}, nil)
		assert.False(t, IsAgentLiveForReconcile(5, s))
	})

	t.Run("recent contact is live", func(t *testing.T) {
		s := store_mocks.NewMockStore(t)
		s.On("AgentFind", int64(5)).Return(&model.Agent{ID: 5, LastContact: time.Now().Unix()}, nil)
		assert.True(t, IsAgentLiveForReconcile(5, s))
	})

	t.Run("contact aged past threshold is not live", func(t *testing.T) {
		s := store_mocks.NewMockStore(t)
		aged := time.Now().Add(-2 * AgentStaleThreshold).Unix()
		s.On("AgentFind", int64(5)).Return(&model.Agent{ID: 5, LastContact: aged}, nil)
		assert.False(t, IsAgentLiveForReconcile(5, s))
	})
}
