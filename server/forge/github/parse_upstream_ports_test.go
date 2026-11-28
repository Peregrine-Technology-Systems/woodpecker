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
	"testing"

	"github.com/google/go-github/v84/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #422, porting upstream woodpecker-ci/woodpecker#7257: a webhook whose body is
// JSON null parsed into a typed nil event, and the hook parsers dereferenced
// it. Gin recovered the panic into a 500; it must be a plain error instead.
func TestParseHookRejectsANullPayload(t *testing.T) {
	for _, event := range []string{hookDeploy, hookPush, hookPull, hookRelease} {
		t.Run(event, func(t *testing.T) {
			var err error
			require.NotPanics(t, func() {
				p, r, b, cc, pc, e := parseHook(testHookRequest([]byte("null"), event), false)
				err = e
				assert.Nil(t, p)
				assert.Nil(t, r)
				assert.Nil(t, b)
				assert.Empty(t, cc)
				assert.Empty(t, pc)
			})
			assert.ErrorContains(t, err, "empty payload")
		})
	}
}

// #422, porting upstream woodpecker-ci/woodpecker#6697: after a force push the
// push's "before" commit is no longer in the branch's history, so listing the
// changed files against it fails. A forced push is treated like a tag, with no
// previous commit, so the changed files come from the head commit alone.
func TestParsePushHookForcedPushHasNoPreviousCommit(t *testing.T) {
	hook := &github.PushEvent{
		Ref:        github.Ptr("refs/heads/main"),
		Before:     github.Ptr("1111111111111111111111111111111111111111"),
		Forced:     github.Ptr(true),
		HeadCommit: &github.HeadCommit{ID: github.Ptr("2222222222222222222222222222222222222222")},
		Repo:       &github.PushEventRepository{FullName: github.Ptr("org/repo")},
	}

	_, pipeline, curr, prev := parsePushHook(hook)

	require.NotNil(t, pipeline)
	assert.Equal(t, "2222222222222222222222222222222222222222", curr)
	assert.Empty(t, prev, "a force push's before commit is not an ancestor")

	hook.Forced = github.Ptr(false)
	_, _, _, prev = parsePushHook(hook)
	assert.Equal(t, "1111111111111111111111111111111111111111", prev, "a normal push keeps its before commit")
}
