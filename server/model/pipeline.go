// Copyright 2021 Woodpecker Authors
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

package model

import (
	"go.woodpecker-ci.org/woodpecker/v3/pipeline/errors"
)

type Pipeline struct {
	ID                   int64                   `json:"id"                      xorm:"pk autoincr 'id'"`
	RepoID               int64                   `json:"-"                       xorm:"UNIQUE(s) INDEX 'repo_id'"`
	Number               int64                   `json:"number"                  xorm:"UNIQUE(s) 'number'"`
	Author               string                  `json:"author"                  xorm:"INDEX 'author'"`
	Parent               int64                   `json:"parent"                  xorm:"parent"`
	Event                WebhookEvent            `json:"event"                   xorm:"event"`
	EventReason          []string                `json:"event_reason"            xorm:"json 'event_reason'"`
	Status               StatusValue             `json:"status"                  xorm:"INDEX 'status'"`
	Errors               []*errors.PipelineError `json:"errors"                  xorm:"json 'errors'"`
	Created              int64                   `json:"created"                 xorm:"'created' NOT NULL DEFAULT 0 created"`
	Updated              int64                   `json:"updated"                 xorm:"'updated' NOT NULL DEFAULT 0 updated"`
	Started              int64                   `json:"started"                 xorm:"started"`
	Finished             int64                   `json:"finished"                xorm:"finished"`
	DeployTo             string                  `json:"deploy_to"               xorm:"deploy"`
	DeployTask           string                  `json:"deploy_task"             xorm:"deploy_task"`
	Commit               string                  `json:"commit"                  xorm:"commit"`
	Branch               string                  `json:"branch"                  xorm:"branch"`
	Ref                  string                  `json:"ref"                     xorm:"ref"`
	Refspec              string                  `json:"refspec"                 xorm:"refspec"`
	Title                string                  `json:"title"                   xorm:"title"`
	Message              string                  `json:"message"                 xorm:"TEXT 'message'"`
	Timestamp            int64                   `json:"timestamp"               xorm:"'timestamp'"`
	Sender               string                  `json:"sender"                  xorm:"sender"` // uses reported user for webhooks and name of cron for cron pipelines
	Avatar               string                  `json:"author_avatar"           xorm:"varchar(500) avatar"`
	Email                string                  `json:"author_email"            xorm:"varchar(500) email"`
	ForgeURL             string                  `json:"forge_url"               xorm:"forge_url"`
	Reviewer             string                  `json:"reviewed_by"             xorm:"reviewer"`
	Reviewed             int64                   `json:"reviewed"                xorm:"reviewed"`
	CancelInfo           *CancelInfo             `json:"cancel_info,omitempty"   xorm:"json 'cancel_info'"`
	Workflows            []*Workflow             `json:"workflows,omitempty"     xorm:"-"`
	ChangedFiles         []string                `json:"changed_files,omitempty" xorm:"LONGTEXT 'changed_files'"`
	AdditionalVariables  map[string]string       `json:"variables,omitempty"     xorm:"json 'additional_variables'"`
	PullRequestLabels    []string                `json:"pr_labels,omitempty"     xorm:"json 'pr_labels'"`
	PullRequestMilestone string                  `json:"pr_milestone,omitempty"  xorm:"pr_milestone"`
	IsPrerelease         bool                    `json:"is_prerelease,omitempty" xorm:"is_prerelease"`
	FromFork             bool                    `json:"from_fork,omitempty"     xorm:"from_fork"`
	// KillReason attributes which code path moved this pipeline into
	// Killed/Canceled/Superseded (#202). Stamped by every transition site:
	// cancel.go::Cancel (user_initiated / superseded_by_newer_push /
	// pending_only_canceled), reconcile.go::killOrphan (reconciler_orphan),
	// rpc.go::release_agent_tasks (agent_disconnect), and external status
	// API (external_status_api). The agent-reported-kill fallback in
	// UpdateStatusToDone splits into agent_preempted (the agent self-canceled
	// because it was terminating — SIGTERM/spot preemption, #275) and the
	// generic agent_done_kill (any other agent-reported kill). Empty for
	// pre-#202 rows; SOC 2 / ISO 27001 attribution + #190 mode-C diagnostics
	// surface.
	KillReason string `json:"kill_reason,omitempty" xorm:"varchar(64) 'kill_reason'"`
	KilledAt   int64  `json:"killed_at,omitempty"   xorm:"INDEX 'killed_at'"`
} //	@name	Pipeline

// TableName return database table name for xorm.
func (Pipeline) TableName() string {
	return "pipelines"
}

type PipelineFilter struct {
	Before      int64
	After       int64
	Branch      string
	Events      []WebhookEvent
	RefContains string
	Status      StatusValue   // single status filter (backward compat)
	Statuses    []StatusValue // multiple status filter (#881)
}

// IsMultiPipeline checks if step list contain more than one parent step.
func (p Pipeline) IsMultiPipeline() bool {
	return len(p.Workflows) > 1
}

// IsPullRequest checks if it's a PR event.
func (p Pipeline) IsPullRequest() bool {
	return p.Event == EventPull || p.Event == EventPullClosed || p.Event == EventPullMetadata
}

type PipelineOptions struct {
	Branch    string            `json:"branch"`
	Variables map[string]string `json:"variables"`
} //	@name	PipelineOptions

type CancelInfo struct {
	CanceledByUser string `json:"canceled_by_user,omitempty"`
	SupersededBy   int64  `json:"superseded_by,omitempty"`
	CanceledByStep string `json:"canceled_by_step,omitempty"`
} //	@name	CancelInfo
