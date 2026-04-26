// Copyright 2026 Peregrine
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0

package pipeline

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	forge_types "go.woodpecker-ci.org/woodpecker/v3/server/forge/types"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
)

// TestRecoverConfigsFromForge_HappyPath (#36): when the database link is
// missing on a restart, freshly-fetched pipeline files must be persisted as
// new Config rows and returned. ConfigPersist is idempotent (hash-based) so
// the recovery is safe to run repeatedly.
func TestRecoverConfigsFromForge_HappyPath(t *testing.T) {
	t.Parallel()
	s := mocks.NewMockStore(t)

	files := []*forge_types.FileMeta{
		{Name: ".woodpecker/deploy.yaml", Data: []byte("when:\n  - event: deployment\n")},
		{Name: ".woodpecker/version-bump.yaml", Data: []byte("when:\n  - event: push\n")},
	}

	// ConfigPersist returns the persisted (or existing) row. We mirror back
	// the input with a fake ID so the caller has something to link.
	s.On("ConfigPersist", mock.MatchedBy(func(c *model.Config) bool {
		return c.RepoID == int64(42) && c.Name == ".woodpecker/deploy.yaml"
	})).Return(&model.Config{ID: 100, RepoID: 42, Name: ".woodpecker/deploy.yaml", Data: files[0].Data}, nil)
	s.On("ConfigPersist", mock.MatchedBy(func(c *model.Config) bool {
		return c.RepoID == int64(42) && c.Name == ".woodpecker/version-bump.yaml"
	})).Return(&model.Config{ID: 101, RepoID: 42, Name: ".woodpecker/version-bump.yaml", Data: files[1].Data}, nil)

	got, err := recoverConfigsFromForge(s, 42, files)
	assert.NoError(t, err)
	assert.Len(t, got, 2)
	assert.Equal(t, int64(100), got[0].ID)
	assert.Equal(t, int64(101), got[1].ID)
}

// TestRecoverConfigsFromForge_PersistError surfaces the underlying store
// error wrapped with a descriptive message — the surrounding Restart logic
// uses that to abort cleanly rather than producing a half-recovered pipeline.
func TestRecoverConfigsFromForge_PersistError(t *testing.T) {
	t.Parallel()
	s := mocks.NewMockStore(t)
	files := []*forge_types.FileMeta{
		{Name: ".woodpecker/deploy.yaml", Data: []byte("when:\n  - event: deployment\n")},
	}
	s.On("ConfigPersist", mock.Anything).Return((*model.Config)(nil), errors.New("disk full"))

	got, err := recoverConfigsFromForge(s, 42, files)
	assert.Nil(t, got)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "deploy.yaml")
	assert.Contains(t, err.Error(), "disk full")
}

// TestRecoverConfigsFromForge_NoFiles is the genuine "definition not found"
// case — when both the database link is missing AND the forge fetch returned
// nothing. Caller surfaces the error; this helper just returns an empty slice.
func TestRecoverConfigsFromForge_NoFiles(t *testing.T) {
	t.Parallel()
	s := mocks.NewMockStore(t)

	got, err := recoverConfigsFromForge(s, 42, nil)
	assert.NoError(t, err)
	assert.Empty(t, got)
	// ConfigPersist must NOT be called when there's nothing to recover.
	s.AssertNotCalled(t, "ConfigPersist", mock.Anything)
}
