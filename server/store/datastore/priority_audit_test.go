// Copyright 2026 Peregrine Technology Systems
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0

package datastore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
)

func TestPriorityAuditInsert_AutoFillsTS(t *testing.T) {
	store, closer := newTestStore(t, new(model.PriorityAudit))
	defer closer()

	row := &model.PriorityAudit{
		ActorEmail:  "amal@pobox.com",
		TaskID:      "task-1",
		OldPriority: 0,
		NewPriority: 5,
	}
	require.NoError(t, store.PriorityAuditInsert(row))

	got, err := store.PriorityAuditList(&model.PriorityAuditListOptions{})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Positive(t, got[0].ID, "id should be auto-assigned")
	assert.NotZero(t, got[0].TS, "ts should be auto-filled by the `created` magic tag")
	assert.Equal(t, "amal@pobox.com", got[0].ActorEmail)
	assert.Equal(t, "task-1", got[0].TaskID)
	assert.EqualValues(t, 0, got[0].OldPriority)
	assert.EqualValues(t, 5, got[0].NewPriority)
}

func TestPriorityAuditList_OrdersByIDDesc(t *testing.T) {
	store, closer := newTestStore(t, new(model.PriorityAudit))
	defer closer()

	for i := 0; i < 5; i++ {
		require.NoError(t, store.PriorityAuditInsert(&model.PriorityAudit{
			ActorEmail:  "u@x.com",
			TaskID:      "task-x",
			OldPriority: int64(i),
			NewPriority: int64(i + 1),
		}))
	}

	got, err := store.PriorityAuditList(&model.PriorityAuditListOptions{})
	require.NoError(t, err)
	require.Len(t, got, 5)
	for i := 0; i < len(got)-1; i++ {
		assert.Greater(t, got[i].ID, got[i+1].ID, "newer rows should come first")
	}
}

func TestPriorityAuditList_LimitClamping(t *testing.T) {
	store, closer := newTestStore(t, new(model.PriorityAudit))
	defer closer()

	for i := 0; i < 10; i++ {
		require.NoError(t, store.PriorityAuditInsert(&model.PriorityAudit{
			ActorEmail: "u@x.com", TaskID: "t",
		}))
	}

	t.Run("default limit is 50", func(t *testing.T) {
		// 10 rows < 50, so we get all 10.
		got, err := store.PriorityAuditList(&model.PriorityAuditListOptions{})
		require.NoError(t, err)
		assert.Len(t, got, 10)
	})

	t.Run("explicit limit honored", func(t *testing.T) {
		got, err := store.PriorityAuditList(&model.PriorityAuditListOptions{Limit: 3})
		require.NoError(t, err)
		assert.Len(t, got, 3)
	})

	t.Run("max limit caps at 200 — stays at 10 because only 10 exist", func(t *testing.T) {
		got, err := store.PriorityAuditList(&model.PriorityAuditListOptions{Limit: 100000})
		require.NoError(t, err)
		assert.Len(t, got, 10)
	})
}

func TestPriorityAuditList_BeforeIDCursor(t *testing.T) {
	store, closer := newTestStore(t, new(model.PriorityAudit))
	defer closer()

	for i := 0; i < 5; i++ {
		require.NoError(t, store.PriorityAuditInsert(&model.PriorityAudit{
			ActorEmail: "u@x.com", TaskID: "t",
		}))
	}

	page1, err := store.PriorityAuditList(&model.PriorityAuditListOptions{Limit: 2})
	require.NoError(t, err)
	require.Len(t, page1, 2)

	page2, err := store.PriorityAuditList(&model.PriorityAuditListOptions{
		Limit:    2,
		BeforeID: page1[len(page1)-1].ID,
	})
	require.NoError(t, err)
	require.Len(t, page2, 2)
	assert.Less(t, page2[0].ID, page1[len(page1)-1].ID, "page2 must start strictly before the cursor")

	// Ensure no overlap between pages.
	seen := map[int64]bool{}
	for _, r := range page1 {
		seen[r.ID] = true
	}
	for _, r := range page2 {
		assert.False(t, seen[r.ID], "page2 row %d should not appear in page1", r.ID)
	}
}

func TestPriorityAuditList_EmptyTableReturnsEmptySlice(t *testing.T) {
	store, closer := newTestStore(t, new(model.PriorityAudit))
	defer closer()

	got, err := store.PriorityAuditList(&model.PriorityAuditListOptions{})
	require.NoError(t, err)
	assert.Empty(t, got)
}
