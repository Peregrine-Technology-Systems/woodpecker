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

package github

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/go-github/v84/github"
	gh_mock "github.com/migueleliasweb/go-github-mock/src/mock"
	"github.com/stretchr/testify/assert"
)

func repoTag(name, sha string) github.RepositoryTag {
	return github.RepositoryTag{
		Name:   github.Ptr(name),
		Commit: &github.Commit{SHA: github.Ptr(sha)},
	}
}

func TestResolveTagSHA_FindsTagOnSecondPage(t *testing.T) {
	// The core bug (#324): the target tag is not on page 1. The old loop never
	// advanced the page, so it would spin on page 1 forever and never see this.
	mockedHTTP := gh_mock.NewMockedHTTPClient(
		gh_mock.WithRequestMatchPages(
			gh_mock.GetReposTagsByOwnerByRepo,
			[]github.RepositoryTag{repoTag("v0.9.0", "aaa"), repoTag("v0.9.1", "bbb")},
			[]github.RepositoryTag{repoTag("v1.0.0", "deadbeef"), repoTag("v1.0.1", "ccc")},
		),
	)
	gh := github.NewClient(mockedHTTP)

	sha, err := resolveTagSHA(t.Context(), gh, "o", "r", "v1.0.0")
	assert.NoError(t, err)
	assert.Equal(t, "deadbeef", sha, "must page past page 1 to find the tag")
}

func TestResolveTagSHA_FindsTagOnFirstPage(t *testing.T) {
	mockedHTTP := gh_mock.NewMockedHTTPClient(
		gh_mock.WithRequestMatch(
			gh_mock.GetReposTagsByOwnerByRepo,
			[]github.RepositoryTag{repoTag("v1.0.0", "deadbeef"), repoTag("v0.9.0", "aaa")},
		),
	)
	gh := github.NewClient(mockedHTTP)

	sha, err := resolveTagSHA(t.Context(), gh, "o", "r", "v1.0.0")
	assert.NoError(t, err)
	assert.Equal(t, "deadbeef", sha)
}

func TestResolveTagSHA_NotFoundTerminatesWithError(t *testing.T) {
	// A tag absent from every page must terminate with an error, NOT loop
	// forever. Run under a hard deadline so a regression back to the infinite
	// loop fails the test instead of hanging CI (#324).
	mockedHTTP := gh_mock.NewMockedHTTPClient(
		gh_mock.WithRequestMatchPages(
			gh_mock.GetReposTagsByOwnerByRepo,
			[]github.RepositoryTag{repoTag("v0.9.0", "aaa")},
			[]github.RepositoryTag{repoTag("v0.9.1", "bbb")},
		),
	)
	gh := github.NewClient(mockedHTTP)

	type result struct {
		sha string
		err error
	}
	done := make(chan result, 1)
	go func() {
		sha, err := resolveTagSHA(context.Background(), gh, "o", "r", "v-does-not-exist")
		done <- result{sha, err}
	}()

	select {
	case r := <-done:
		assert.Error(t, r.err, "absent tag must return an error")
		assert.Contains(t, r.err.Error(), "could not find tag")
		assert.Empty(t, r.sha)
	case <-time.After(3 * time.Second):
		t.Fatal("resolveTagSHA did not terminate — pagination regressed to the infinite loop (#324)")
	}
}

func TestResolveTagSHA_ForgeErrorPropagates(t *testing.T) {
	// A permanent forge error (here a 404) surfaces to the caller rather than
	// being swallowed or spun on.
	mockedHTTP := gh_mock.NewMockedHTTPClient(
		gh_mock.WithRequestMatchHandler(
			gh_mock.GetReposTagsByOwnerByRepo,
			http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"Not Found"}`))
			}),
		),
	)
	gh := github.NewClient(mockedHTTP)

	sha, err := resolveTagSHA(t.Context(), gh, "o", "r", "v1.0.0")
	assert.Error(t, err)
	assert.Empty(t, sha)
}
