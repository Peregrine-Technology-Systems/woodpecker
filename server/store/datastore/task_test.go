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

package datastore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
)

func TestTaskList(t *testing.T) {
	store, closer := newTestStore(t, new(model.Task))
	defer closer()

	assert.NoError(t, store.TaskInsert(&model.Task{
		ID:        "some_random_id",
		Data:      []byte("foo"),
		Labels:    map[string]string{"foo": "bar"},
		DepStatus: map[string]model.StatusValue{"test": "dep"},
	}))

	list, err := store.TaskList()
	assert.NoError(t, err)
	assert.Len(t, list, 1, "Expected one task in list")
	assert.Equal(t, "some_random_id", list[0].ID)
	assert.Equal(t, "foo", string(list[0].Data))
	assert.EqualValues(t, map[string]model.StatusValue{"test": "dep"}, list[0].DepStatus)

	assert.NoError(t, store.TaskDelete("some_random_id"))

	list, err = store.TaskList()
	assert.NoError(t, err)
	assert.Len(t, list, 0, "Want empty task list after delete")
}

// TestTaskList_OrdersByPriorityDescThenCreatedAsc encodes the #45 contract
// for the persistent-queue restore path: tasks must come out priority-high
// first, FIFO inside a priority bucket.
//
// Uses raw INSERT to bypass xorm's `created` magic — the production code
// path (TaskInsert) deliberately auto-fills Created via time.Now() and that
// stamping cannot be overridden from the application layer, but the
// persistent-restore ordering must still be deterministic.
func TestTaskList_OrdersByPriorityDescThenCreatedAsc(t *testing.T) {
	store, closer := newTestStore(t, new(model.Task))
	defer closer()

	type row struct {
		id                string
		priority, created int64
	}
	for _, r := range []row{
		{"low-old", 0, 100},
		{"high-new", 5, 300},
		{"low-new", 0, 200},
		{"high-old", 5, 200},
	} {
		_, err := store.engine.Exec(
			"INSERT INTO tasks (id, priority, created) VALUES (?, ?, ?)",
			r.id, r.priority, r.created)
		require.NoError(t, err)
	}

	list, err := store.TaskList()
	require.NoError(t, err)
	require.Len(t, list, 4)

	got := []string{list[0].ID, list[1].ID, list[2].ID, list[3].ID}
	want := []string{"high-old", "high-new", "low-old", "low-new"}
	assert.Equal(t, want, got, "priority DESC, created ASC")
}

// TestTaskList_DefaultPriorityIsZero ensures that an existing-pattern insert
// without setting Priority preserves the FIFO contract — the column is
// NOT NULL DEFAULT 0 so a zero-value task lands at the back of every
// non-zero-priority bucket but in front of any later inserts at priority 0.
func TestTaskList_DefaultPriorityIsZero(t *testing.T) {
	store, closer := newTestStore(t, new(model.Task))
	defer closer()

	require.NoError(t, store.TaskInsert(&model.Task{ID: "default-priority", Created: 1}))

	list, err := store.TaskList()
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.EqualValues(t, 0, list[0].Priority)
}

// TestTaskInsert_AutoFillsCreated verifies the xorm `created` magic tag is
// wired up — the application-layer TaskInsert path must populate Created
// without callers having to remember.
func TestTaskInsert_AutoFillsCreated(t *testing.T) {
	store, closer := newTestStore(t, new(model.Task))
	defer closer()

	task := &model.Task{ID: "auto-filled"}
	require.NoError(t, store.TaskInsert(task))

	list, err := store.TaskList()
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.NotZero(t, list[0].Created, "xorm `created` tag should fill on insert")
}

// TestUpdateTaskPriority_OnlyPriorityColumn locks in the #47 contract: the
// UpdateTaskPriority store method must touch only the priority column,
// never AgentID/PipelineID/etc.
func TestUpdateTaskPriority_OnlyPriorityColumn(t *testing.T) {
	store, closer := newTestStore(t, new(model.Task))
	defer closer()

	require.NoError(t, store.TaskInsert(&model.Task{
		ID:         "task-uta",
		AgentID:    42,
		PipelineID: 99,
		RepoID:     7,
		Priority:   0,
	}))

	require.NoError(t, store.UpdateTaskPriority("task-uta", 10))

	list, err := store.TaskList()
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.EqualValues(t, 10, list[0].Priority, "priority should be updated")
	assert.EqualValues(t, 42, list[0].AgentID, "AgentID must be untouched")
	assert.EqualValues(t, 99, list[0].PipelineID, "PipelineID must be untouched")
	assert.EqualValues(t, 7, list[0].RepoID, "RepoID must be untouched")
}

// TestUpdateTaskPriority_UnknownTaskNoOp — UPDATE with no matching rows
// is not an error; the in-memory queue is the source of truth for
// "task exists / is pending", so a stale or already-deleted DB row
// shouldn't fail the handler path.
func TestUpdateTaskPriority_UnknownTaskNoOp(t *testing.T) {
	store, closer := newTestStore(t, new(model.Task))
	defer closer()

	assert.NoError(t, store.UpdateTaskPriority("does-not-exist", 5))
}
