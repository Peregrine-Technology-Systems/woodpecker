// Copyright 2022 Woodpecker Authors
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

package github

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/google/go-github/v84/github"
	"github.com/stretchr/testify/assert"

	"go.woodpecker-ci.org/woodpecker/v3/server/forge/github/fixtures"
	"go.woodpecker-ci.org/woodpecker/v3/server/forge/types"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
)

const (
	hookEvent   = "X-GitHub-Event"
	hookDeploy  = "deployment"
	hookPush    = "push"
	hookPull    = "pull_request"
	hookRelease = "release"
)

func testHookRequest(payload []byte, event string) *http.Request {
	buf := bytes.NewBuffer(payload)
	req, _ := http.NewRequest(http.MethodPost, "/hook", buf)
	req.Header = http.Header{}
	req.Header.Set(hookEvent, event)
	return req
}

func Test_parseHook(t *testing.T) {
	t.Run("ignore unsupported hook events", func(t *testing.T) {
		req := testHookRequest([]byte(fixtures.HookPullRequest), "issues")
		p, r, b, cc, pc, err := parseHook(req, false)
		assert.Empty(t, pc)
		assert.Empty(t, cc)
		assert.Nil(t, r)
		assert.Nil(t, b)
		assert.Nil(t, p)
		assert.ErrorIs(t, err, &types.ErrIgnoreEvent{})
	})

	t.Run("skip skip push hook when action is deleted", func(t *testing.T) {
		req := testHookRequest([]byte(fixtures.HookPushDeleted), hookPush)
		p, r, b, cc, pc, err := parseHook(req, false)
		assert.Empty(t, pc)
		assert.Empty(t, cc)
		assert.Nil(t, r)
		assert.Nil(t, b)
		assert.NoError(t, err)
		assert.Nil(t, p)
	})
	t.Run("push hook", func(t *testing.T) {
		req := testHookRequest([]byte(fixtures.HookPush), hookPush)
		p, r, b, cc, pc, err := parseHook(req, false)
		assert.Equal(t, "2f780193b136b72bfea4eeb640786a8c4450c7a2", pc)
		assert.Equal(t, "366701fde727cb7a9e7f21eb88264f59f6f9b89c", cc)
		assert.NoError(t, err)
		assert.Nil(t, p)
		assert.NotNil(t, r)
		assert.NotNil(t, b)
		assert.Equal(t, model.EventPush, b.Event)
		sort.Strings(b.ChangedFiles)
	})

	t.Run("PR hook", func(t *testing.T) {
		req := testHookRequest([]byte(fixtures.HookPullRequest), hookPull)
		p, r, b, cc, pc, err := parseHook(req, false)
		assert.Empty(t, pc)
		assert.Empty(t, cc)
		assert.NoError(t, err)
		assert.NotNil(t, r)
		assert.NotNil(t, b)
		assert.NotNil(t, p)
		assert.Equal(t, model.EventPull, b.Event)
	})
	t.Run("PR closed hook", func(t *testing.T) {
		req := testHookRequest([]byte(fixtures.HookPullRequestClosed), hookPull)
		p, r, b, cc, pc, err := parseHook(req, false)
		assert.Empty(t, pc)
		assert.Empty(t, cc)
		assert.NoError(t, err)
		assert.NotNil(t, r)
		assert.NotNil(t, b)
		assert.NotNil(t, p)
		assert.Equal(t, model.EventPullClosed, b.Event)
	})

	t.Run("reopen a pull", func(t *testing.T) {
		req := testHookRequest([]byte(fixtures.HookPullRequestReopened), hookPull)
		p, r, b, cc, pc, err := parseHook(req, false)
		assert.Empty(t, pc)
		assert.Empty(t, cc)
		assert.NoError(t, err)
		assert.NotNil(t, r)
		assert.NotNil(t, b)
		assert.NotNil(t, p)
		assert.Equal(t, model.EventPull, b.Event)
	})

	t.Run("PR merged hook", func(t *testing.T) {
		req := testHookRequest([]byte(fixtures.HookPullRequestMerged), hookPull)
		p, r, b, cc, pc, err := parseHook(req, false)
		assert.Empty(t, pc)
		assert.Empty(t, cc)
		assert.NoError(t, err)
		assert.NotNil(t, r)
		assert.NotNil(t, b)
		assert.NotNil(t, p)
		assert.Equal(t, model.EventPullClosed, b.Event)
	})

	t.Run("PR edited hook", func(t *testing.T) {
		req := testHookRequest([]byte(fixtures.HookPullRequestEdited), hookPull)
		p, r, b, cc, pc, err := parseHook(req, false)
		assert.Empty(t, pc)
		assert.Empty(t, cc)
		assert.NoError(t, err)
		assert.NotNil(t, r)
		assert.NotNil(t, b)
		assert.NotNil(t, p)
		assert.Equal(t, model.EventPullMetadata, b.Event)
		assert.Equal(t, []string{"edited"}, b.EventReason)
	})

	t.Run("deploy hook", func(t *testing.T) {
		req := testHookRequest([]byte(fixtures.HookDeploy), hookDeploy)
		p, r, b, cc, pc, err := parseHook(req, false)
		assert.Empty(t, pc)
		assert.Empty(t, cc)
		assert.NoError(t, err)
		assert.NotNil(t, r)
		assert.NotNil(t, b)
		assert.Nil(t, p)
		assert.Equal(t, model.EventDeploy, b.Event)
		assert.Equal(t, "production", b.DeployTo)
		assert.Equal(t, "deploy", b.DeployTask)
	})

	t.Run("release hook", func(t *testing.T) {
		req := testHookRequest([]byte(fixtures.HookRelease), hookRelease)
		p, r, b, cc, pc, err := parseHook(req, false)
		assert.Empty(t, pc)
		assert.Empty(t, cc)
		assert.NoError(t, err)
		assert.NotNil(t, r)
		assert.NotNil(t, b)
		assert.Nil(t, p)
		assert.Equal(t, model.EventRelease, b.Event)
		assert.Len(t, strings.Split(b.Ref, "/"), 3)
		assert.True(t, strings.HasPrefix(b.Ref, "refs/tags/"))
	})

	t.Run("pull review requested", func(t *testing.T) {
		req := testHookRequest([]byte(fixtures.HookPullRequestReviewRequested), hookPull)
		p, r, b, cc, pc, err := parseHook(req, false)
		assert.Empty(t, pc)
		assert.Empty(t, cc)
		assert.ErrorIs(t, err, &types.ErrIgnoreEvent{})
		assert.Nil(t, r)
		assert.Nil(t, b)
		assert.Nil(t, p)
	})

	t.Run("pull milestoned", func(t *testing.T) {
		req := testHookRequest([]byte(fixtures.HookPullRequestMilestoneAdded), hookPull)
		p, r, b, cc, pc, err := parseHook(req, false)
		assert.Empty(t, pc)
		assert.Empty(t, cc)
		assert.NoError(t, err)
		assert.NotNil(t, r)
		assert.NotNil(t, b)
		assert.Equal(t, model.EventPullMetadata, b.Event)
		assert.Equal(t, []string{"milestoned"}, b.EventReason)
		if assert.NotNil(t, p) {
			assert.Equal(t, int64(2705176047), *p.ID)
			assert.Equal(t, 1, *p.Number)
			assert.Equal(t, "open", *p.State)
			assert.Equal(t, "Some ned more AAAA", *p.Title)
			assert.Equal(t, "yeaaa", *p.Body)
			assert.Equal(t, false, *p.Draft)
			assert.Equal(t, false, *p.Merged)
			assert.Equal(t, true, *p.Mergeable)
			assert.Equal(t, "unstable", *p.MergeableState)
			if assert.NotNil(t, p.User) {
				assert.Equal(t, "6543", *p.User.Login)
				assert.Equal(t, int64(24977596), *p.User.ID)
			}
			if assert.NotNil(t, p.Milestone) {
				assert.Equal(t, int64(13392101), *p.Milestone.ID)
				assert.Equal(t, 2, *p.Milestone.Number)
				assert.Equal(t, "open mile", *p.Milestone.Title)
				assert.Equal(t, "ongoing", *p.Milestone.Description)
				assert.Equal(t, "open", *p.Milestone.State)
				if assert.NotNil(t, p.Milestone.Creator) {
					assert.Equal(t, "demoaccount2-commits", *p.Milestone.Creator.Login)
					assert.Equal(t, int64(223550959), *p.Milestone.Creator.ID)
				}
			}
			assert.Empty(t, p.RequestedReviewers)
			if assert.NotNil(t, p.Head) {
				assert.Equal(t, "6543-patch-1", *p.Head.Ref)
				assert.Equal(t, "36b5813240a9d2daa29b05046d56a53e18f39a3e", *p.Head.SHA)
			}
			if assert.NotNil(t, p.Base) {
				assert.Equal(t, "main", *p.Base.Ref)
				assert.Equal(t, "67012991d6c69b1c58378346fca366b864d8d1a1", *p.Base.SHA)
			}
		}
	})

	// milestone change will result two webhooks an demilestoned and milestoned

	t.Run("pull request demilestoned", func(t *testing.T) {
		req := testHookRequest([]byte(fixtures.HookPullRequestMilestoneRemoved), hookPull)
		p, r, b, cc, pc, err := parseHook(req, false)
		assert.Empty(t, pc)
		assert.Empty(t, cc)
		assert.NoError(t, err)
		assert.NotNil(t, r)
		assert.NotNil(t, b)
		assert.Equal(t, model.EventPullMetadata, b.Event)
		assert.Equal(t, []string{"demilestoned"}, b.EventReason)
		if assert.NotNil(t, p) {
			assert.Equal(t, int64(2705176047), *p.ID)
			assert.Equal(t, 1, *p.Number)
			assert.Equal(t, "open", *p.State)
			assert.Equal(t, "Some ned more AAAA", *p.Title)
			if assert.NotNil(t, p.User) {
				assert.Equal(t, "6543", *p.User.Login)
				assert.Equal(t, int64(24977596), *p.User.ID)
			}
			if assert.Len(t, p.Labels, 1) {
				assert.Equal(t, int64(9024465370), *p.Labels[0].ID)
				assert.Equal(t, "bug", *p.Labels[0].Name)
				assert.Equal(t, "d73a4a", *p.Labels[0].Color)
				assert.Equal(t, "Something isn't working", *p.Labels[0].Description)
			}
			assert.Nil(t, p.Milestone)
			if assert.NotNil(t, p.Head) {
				assert.Equal(t, "6543-patch-1", *p.Head.Ref)
				assert.Equal(t, "36b5813240a9d2daa29b05046d56a53e18f39a3e", *p.Head.SHA)
			}
			if assert.NotNil(t, p.Base) {
				assert.Equal(t, "main", *p.Base.Ref)
				assert.Equal(t, "67012991d6c69b1c58378346fca366b864d8d1a1", *p.Base.SHA)
			}
		}
	})

	t.Run("pull request labele added", func(t *testing.T) {
		req := testHookRequest([]byte(fixtures.HookPullRequestLabelAdded), hookPull)
		p, r, b, cc, pc, err := parseHook(req, false)
		assert.Empty(t, pc)
		assert.Empty(t, cc)
		assert.NoError(t, err)
		assert.NotNil(t, r)
		assert.NotNil(t, b)
		assert.Equal(t, model.EventPullMetadata, b.Event)
		assert.Equal(t, []string{"label_updated"}, b.EventReason)
		if assert.NotNil(t, p) {
			assert.Equal(t, int64(2705176047), *p.ID)
			assert.Equal(t, 1, *p.Number)
			assert.Equal(t, "open", *p.State)
			assert.Equal(t, "Some ned more AAAA", *p.Title)
			assert.Equal(t, "yeaaa", *p.Body)
			assert.Equal(t, false, *p.Draft)
			assert.Equal(t, false, *p.Merged)
			assert.Equal(t, true, *p.Mergeable)
			assert.Equal(t, "unstable", *p.MergeableState)
			if assert.NotNil(t, p.User) {
				assert.Equal(t, "6543", *p.User.Login)
				assert.Equal(t, int64(24977596), *p.User.ID)
			}
			if assert.Len(t, p.Labels, 2) {
				assert.Equal(t, int64(9024465376), *p.Labels[0].ID)
				assert.Equal(t, "documentation", *p.Labels[0].Name)
				assert.Equal(t, "0075ca", *p.Labels[0].Color)
				assert.Equal(t, "Improvements or additions to documentation", *p.Labels[0].Description)
				assert.Equal(t, int64(9024465382), *p.Labels[1].ID)
				assert.Equal(t, "enhancement", *p.Labels[1].Name)
				assert.Equal(t, "a2eeef", *p.Labels[1].Color)
				assert.Equal(t, "New feature or request", *p.Labels[1].Description)
			}
			if assert.NotNil(t, p.Milestone) {
				assert.Equal(t, int64(13392101), *p.Milestone.ID)
				assert.Equal(t, "open mile", *p.Milestone.Title)
			}
			assert.Empty(t, p.RequestedReviewers)
			if assert.NotNil(t, p.Head) {
				assert.Equal(t, "6543-patch-1", *p.Head.Ref)
				assert.Equal(t, "36b5813240a9d2daa29b05046d56a53e18f39a3e", *p.Head.SHA)
			}
			if assert.NotNil(t, p.Base) {
				assert.Equal(t, "main", *p.Base.Ref)
				assert.Equal(t, "67012991d6c69b1c58378346fca366b864d8d1a1", *p.Base.SHA)
			}
		}
	})

	// lable change will result two webhooks an unlable and labeled

	t.Run("pull request got label removed", func(t *testing.T) {
		req := testHookRequest([]byte(fixtures.HookPullRequestLabelRemoved), hookPull)
		p, r, b, cc, pc, err := parseHook(req, false)
		assert.Empty(t, pc)
		assert.Empty(t, cc)
		assert.NoError(t, err)
		assert.NotNil(t, r)
		assert.NotNil(t, b)
		assert.Equal(t, model.EventPullMetadata, b.Event)
		assert.Equal(t, []string{"label_updated"}, b.EventReason)
		if assert.NotNil(t, p) {
			assert.Equal(t, int64(2705176047), *p.ID)
			assert.Equal(t, 1, *p.Number)
			assert.Equal(t, "open", *p.State)
			assert.Equal(t, "Some ned more AAAA", *p.Title)
			if assert.NotNil(t, p.User) {
				assert.Equal(t, "6543", *p.User.Login)
				assert.Equal(t, int64(24977596), *p.User.ID)
			}
			if assert.Len(t, p.Labels, 1) {
				assert.Equal(t, int64(9024465370), *p.Labels[0].ID)
				assert.Equal(t, "bug", *p.Labels[0].Name)
				assert.Equal(t, "d73a4a", *p.Labels[0].Color)
				assert.Equal(t, "Something isn't working", *p.Labels[0].Description)
			}
			if assert.NotNil(t, p.Head) {
				assert.Equal(t, "6543-patch-1", *p.Head.Ref)
				assert.Equal(t, "36b5813240a9d2daa29b05046d56a53e18f39a3e", *p.Head.SHA)
			}
			if assert.NotNil(t, p.Base) {
				assert.Equal(t, "main", *p.Base.Ref)
				assert.Equal(t, "67012991d6c69b1c58378346fca366b864d8d1a1", *p.Base.SHA)
			}
		}
	})

	t.Run("pull request got all label removed", func(t *testing.T) {
		req := testHookRequest([]byte(fixtures.HookPullRequestLabelsCleared), hookPull)
		p, r, b, cc, pc, err := parseHook(req, false)
		assert.Empty(t, pc)
		assert.Empty(t, cc)
		assert.NoError(t, err)
		assert.NotNil(t, r)
		assert.NotNil(t, b)
		assert.Equal(t, model.EventPullMetadata, b.Event)
		assert.Equal(t, []string{"label_cleared"}, b.EventReason)
		if assert.NotNil(t, p) {
			assert.Equal(t, int64(2705176047), *p.ID)
			assert.Equal(t, 1, *p.Number)
			assert.Equal(t, "open", *p.State)
			assert.Equal(t, "Some ned more AAAA", *p.Title)
			assert.Empty(t, p.Labels)
			if assert.NotNil(t, p.User) {
				assert.Equal(t, "6543", *p.User.Login)
				assert.Equal(t, int64(24977596), *p.User.ID)
			}
			if assert.NotNil(t, p.Head) {
				assert.Equal(t, "6543-patch-1", *p.Head.Ref)
				assert.Equal(t, "36b5813240a9d2daa29b05046d56a53e18f39a3e", *p.Head.SHA)
			}
			if assert.NotNil(t, p.Base) {
				assert.Equal(t, "main", *p.Base.Ref)
				assert.Equal(t, "67012991d6c69b1c58378346fca366b864d8d1a1", *p.Base.SHA)
			}
		}
	})

	t.Run("pull request assigned", func(t *testing.T) {
		req := testHookRequest([]byte(fixtures.HookPullRequestAssigneeAdded), hookPull)
		p, r, b, cc, pc, err := parseHook(req, false)
		assert.Empty(t, pc)
		assert.Empty(t, cc)
		assert.NoError(t, err)
		assert.NotNil(t, r)
		assert.NotNil(t, b)
		assert.Equal(t, model.EventPullMetadata, b.Event)
		assert.Equal(t, []string{"assigned"}, b.EventReason)
		if assert.NotNil(t, p) {
			assert.Equal(t, int64(2705176047), *p.ID)
			assert.Equal(t, 1, *p.Number)
			assert.Equal(t, "open", *p.State)
			assert.Equal(t, "Some ned more AAAA", *p.Title)
			if assert.NotNil(t, p.User) {
				assert.Equal(t, "6543", *p.User.Login)
				assert.Equal(t, int64(24977596), *p.User.ID)
			}
			if assert.NotNil(t, p.Assignee) {
				assert.Equal(t, "demoaccount2-commits", *p.Assignee.Login)
				assert.Equal(t, int64(223550959), *p.Assignee.ID)
			}
			if assert.Len(t, p.Assignees, 1) {
				assert.Equal(t, "demoaccount2-commits", *p.Assignees[0].Login)
				assert.Equal(t, int64(223550959), *p.Assignees[0].ID)
			}
			if assert.Len(t, p.Labels, 1) {
				assert.Equal(t, int64(9024465370), *p.Labels[0].ID)
				assert.Equal(t, "bug", *p.Labels[0].Name)
			}
			assert.Nil(t, p.Milestone)
			if assert.NotNil(t, p.Head) {
				assert.Equal(t, "6543-patch-1", *p.Head.Ref)
				assert.Equal(t, "36b5813240a9d2daa29b05046d56a53e18f39a3e", *p.Head.SHA)
			}
			if assert.NotNil(t, p.Base) {
				assert.Equal(t, "main", *p.Base.Ref)
				assert.Equal(t, "67012991d6c69b1c58378346fca366b864d8d1a1", *p.Base.SHA)
			}
		}
	})

	// assigne change will result two webhooks an assigned and unassigned

	t.Run("pull request unassigned", func(t *testing.T) {
		req := testHookRequest([]byte(fixtures.HookPullRequestAssigneeRemoved), hookPull)
		p, r, b, cc, pc, err := parseHook(req, false)
		assert.Empty(t, pc)
		assert.Empty(t, cc)
		assert.NoError(t, err)
		assert.NotNil(t, r)
		assert.NotNil(t, b)
		assert.Equal(t, model.EventPullMetadata, b.Event)
		assert.Equal(t, []string{"unassigned"}, b.EventReason)
		if assert.NotNil(t, p) {
			assert.Equal(t, int64(2705176047), *p.ID)
			assert.Equal(t, 1, *p.Number)
			assert.Equal(t, "open", *p.State)
			assert.Equal(t, "Some ned more AAAA", *p.Title)
			if assert.NotNil(t, p.User) {
				assert.Equal(t, "6543", *p.User.Login)
				assert.Equal(t, int64(24977596), *p.User.ID)
			}
			assert.Nil(t, p.Assignee)
			assert.Empty(t, p.Assignees)
			if assert.Len(t, p.Labels, 1) {
				assert.Equal(t, int64(9024465370), *p.Labels[0].ID)
				assert.Equal(t, "bug", *p.Labels[0].Name)
			}
			assert.Nil(t, p.Milestone)
			if assert.NotNil(t, p.Head) {
				assert.Equal(t, "6543-patch-1", *p.Head.Ref)
				assert.Equal(t, "36b5813240a9d2daa29b05046d56a53e18f39a3e", *p.Head.SHA)
			}
			if assert.NotNil(t, p.Base) {
				assert.Equal(t, "main", *p.Base.Ref)
				assert.Equal(t, "67012991d6c69b1c58378346fca366b864d8d1a1", *p.Base.SHA)
			}
		}
	})
}

// testHookRequestForm builds a request the way GitHub sends it when the webhook
// is configured with content_type: form — the JSON payload percent-encoded into
// a single "payload" application/x-www-form-urlencoded field (#301).
func testHookRequestForm(payload []byte, event string) *http.Request {
	form := url.Values{}
	form.Set(hookField, string(payload))
	body := form.Encode()
	req, _ := http.NewRequest(http.MethodPost, "/hook", strings.NewReader(body))
	req.Header = http.Header{}
	req.Header.Set(hookEvent, event)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

func Test_parseHook_formEncoding(t *testing.T) {
	t.Run("form-encoded payload parses same as raw JSON", func(t *testing.T) {
		req := testHookRequestForm([]byte(fixtures.HookPush), hookPush)
		p, r, b, cc, pc, err := parseHook(req, false)
		assert.NoError(t, err)
		assert.Nil(t, p)
		assert.NotNil(t, r)
		assert.NotNil(t, b)
		assert.Equal(t, model.EventPush, b.Event)
		assert.Equal(t, "2f780193b136b72bfea4eeb640786a8c4450c7a2", pc)
		assert.Equal(t, "366701fde727cb7a9e7f21eb88264f59f6f9b89c", cc)
	})

	t.Run("form-encoded body missing the payload field fails loudly", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "/hook", strings.NewReader("not_payload=whatever"))
		req.Header = http.Header{}
		req.Header.Set(hookEvent, hookPush)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		p, r, b, cc, pc, err := parseHook(req, false)
		assert.Empty(t, pc)
		assert.Empty(t, cc)
		assert.Nil(t, r)
		assert.Nil(t, b)
		assert.Nil(t, p)
		if assert.Error(t, err) {
			assert.Contains(t, err.Error(), `missing "payload" field`)
		}
	})

	t.Run("malformed percent-encoding fails loudly instead of silently falling through", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "/hook", strings.NewReader("payload=%zz"))
		req.Header = http.Header{}
		req.Header.Set(hookEvent, hookPush)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		p, r, b, cc, pc, err := parseHook(req, false)
		assert.Empty(t, pc)
		assert.Empty(t, cc)
		assert.Nil(t, r)
		assert.Nil(t, b)
		assert.Nil(t, p)
		if assert.Error(t, err) {
			assert.Contains(t, err.Error(), "failed to parse form-encoded webhook body")
		}
	})

	t.Run("oversized body fails loudly instead of being silently truncated", func(t *testing.T) {
		orig := maxHookBodySize
		maxHookBodySize = 16
		defer func() { maxHookBodySize = orig }()

		req := testHookRequestForm([]byte(fixtures.HookPush), hookPush)
		p, r, b, cc, pc, err := parseHook(req, false)
		assert.Empty(t, pc)
		assert.Empty(t, cc)
		assert.Nil(t, r)
		assert.Nil(t, b)
		assert.Nil(t, p)
		if assert.Error(t, err) {
			assert.Contains(t, err.Error(), "exceeds max size")
		}
	})

	t.Run("oversized raw JSON body fails loudly (non-form content type)", func(t *testing.T) {
		orig := maxHookBodySize
		maxHookBodySize = 16
		defer func() { maxHookBodySize = orig }()

		req := testHookRequest([]byte(fixtures.HookPush), hookPush)
		p, r, b, cc, pc, err := parseHook(req, false)
		assert.Empty(t, pc)
		assert.Empty(t, cc)
		assert.Nil(t, r)
		assert.Nil(t, b)
		assert.Nil(t, p)
		if assert.Error(t, err) {
			assert.Contains(t, err.Error(), "exceeds max size")
		}
	})

	t.Run("body read failure fails loudly", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "/hook", io.NopCloser(iotest.ErrReader(errors.New("connection reset"))))
		req.Header = http.Header{}
		req.Header.Set(hookEvent, hookPush)

		p, r, b, cc, pc, err := parseHook(req, false)
		assert.Empty(t, pc)
		assert.Empty(t, cc)
		assert.Nil(t, r)
		assert.Nil(t, b)
		assert.Nil(t, p)
		if assert.Error(t, err) {
			assert.Contains(t, err.Error(), "failed to read webhook request body")
		}
	})
}

// The helper-function tests below cover pre-existing branches in this file that
// were not exercised by the fixture-driven Test_parseHook cases above. Touching
// this file for #301/#302 puts it under the 95% per-file coverage gate, so these
// close gaps in parsePushHook/parseDeployHook/parsePullHook/parseReleaseHook that
// predate this change.

func Test_parsePushHook_authorFallback(t *testing.T) {
	hook := &github.PushEvent{
		Ref:        github.Ptr("refs/heads/main"),
		Before:     github.Ptr("aaa"),
		Repo:       &github.PushEventRepository{FullName: github.Ptr("octocat/hello")},
		Sender:     &github.User{Login: github.Ptr("")}, // no sender login -> fall back to commit author
		HeadCommit: &github.HeadCommit{ID: github.Ptr("bbb"), Author: &github.CommitAuthor{Login: github.Ptr("commit-author")}},
	}
	repo, pipeline, curr, prev := parsePushHook(hook)
	assert.NotNil(t, repo)
	assert.Equal(t, "commit-author", pipeline.Author)
	assert.Equal(t, "bbb", curr)
	assert.Equal(t, "aaa", prev)
}

func Test_parseDeployHook_refNormalization(t *testing.T) {
	t.Run("sha-like ref falls back to repo default branch", func(t *testing.T) {
		hook := &github.DeploymentEvent{
			Deployment: &github.Deployment{
				SHA: github.Ptr("deadbeef"),
				Ref: github.Ptr("deadbeef"), // ref == commit -> reconstruct from default branch
			},
			Repo: &github.Repository{DefaultBranch: github.Ptr("main")},
		}
		_, pipeline := parseDeployHook(hook)
		assert.Equal(t, "main", pipeline.Branch)
		assert.Equal(t, "refs/heads/main", pipeline.Ref)
	})

	t.Run("bare branch name gets refs/heads prefix", func(t *testing.T) {
		hook := &github.DeploymentEvent{
			Deployment: &github.Deployment{
				SHA: github.Ptr("deadbeef"),
				Ref: github.Ptr("staging"), // not a sha match, not already refs/-prefixed
			},
			Repo: &github.Repository{DefaultBranch: github.Ptr("main")},
		}
		_, pipeline := parseDeployHook(hook)
		assert.Equal(t, "refs/heads/staging", pipeline.Ref)
	})
}

func Test_parsePullHook_unsupportedAction(t *testing.T) {
	hook := &github.PullRequestEvent{
		Action:      github.Ptr("review_request_removed"),
		PullRequest: &github.PullRequest{Number: github.Ptr(1)},
		Repo:        &github.Repository{FullName: github.Ptr("octocat/hello")},
	}
	pr, repo, pipeline, err := parsePullHook(hook, false)
	assert.Nil(t, pr)
	assert.Nil(t, repo)
	assert.Nil(t, pipeline)
	assert.ErrorIs(t, err, &types.ErrIgnoreEvent{})
}

func Test_parsePullHook_merge(t *testing.T) {
	hook := &github.PullRequestEvent{
		Action: github.Ptr(actionOpen),
		PullRequest: &github.PullRequest{
			Number: github.Ptr(42),
			Head:   &github.PullRequestBranch{Ref: github.Ptr("feature"), SHA: github.Ptr("abc"), Repo: &github.Repository{ID: github.Ptr(int64(1))}},
			Base:   &github.PullRequestBranch{Ref: github.Ptr("main"), Repo: &github.Repository{ID: github.Ptr(int64(1))}},
		},
		Repo: &github.Repository{FullName: github.Ptr("octocat/hello")},
	}
	_, _, pipeline, err := parsePullHook(hook, true)
	assert.NoError(t, err)
	assert.Equal(t, fmt.Sprintf(mergeRefs, 42), pipeline.Ref)
}

func Test_parseReleaseHook(t *testing.T) {
	t.Run("non-released action is ignored", func(t *testing.T) {
		hook := &github.ReleaseEvent{
			Action:  github.Ptr("edited"),
			Release: &github.RepositoryRelease{TagName: github.Ptr("v1.0.0")},
		}
		repo, pipeline := parseReleaseHook(hook)
		assert.Nil(t, repo)
		assert.Nil(t, pipeline)
	})

	t.Run("empty release name falls back to tag name", func(t *testing.T) {
		hook := &github.ReleaseEvent{
			Action: github.Ptr(actionReleased),
			Release: &github.RepositoryRelease{
				TagName: github.Ptr("v1.0.0"),
				Name:    github.Ptr(""),
			},
			Repo: &github.Repository{FullName: github.Ptr("octocat/hello")},
		}
		repo, pipeline := parseReleaseHook(hook)
		assert.NotNil(t, repo)
		assert.Equal(t, "created release v1.0.0", pipeline.Message)
	})
}
