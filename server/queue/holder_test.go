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

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
)

// #418: the queue is the authority for which agent holds a running workflow.
func TestFifoHolder(t *testing.T) {
	ctx, _, q := setupTestQueue(t)
	require.NoError(t, q.PushAtOnce(ctx, []*model.Task{{ID: "held-1"}}))

	_, err := q.Holder("held-1")
	assert.ErrorIs(t, err, ErrNotFound, "a pending task has no holder")

	waitForProcess()
	got, err := q.Poll(ctx, 7, filterFnTrue)
	require.NoError(t, err)

	holder, err := q.Holder(got.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(7), holder)

	require.NoError(t, q.Done(ctx, got.ID, model.StatusSuccess))
	_, err = q.Holder(got.ID)
	assert.ErrorIs(t, err, ErrNotFound, "a finished task has no holder")

	_, err = q.Holder("never-pushed")
	assert.ErrorIs(t, err, ErrNotFound)
}
