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

package datastore

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/store/types"
)

func TestPipelines(t *testing.T) {
	repo := &model.Repo{
		UserID:   1,
		FullName: "bradrydzewski/test",
		Owner:    "bradrydzewski",
		Name:     "test",
	}

	store, closer := newTestStore(t, new(model.Repo), new(model.Step), new(model.Pipeline))
	defer closer()

	assert.NoError(t, store.CreateRepo(repo))

	// Fail when the repo is not existing
	pipeline := model.Pipeline{
		RepoID: 100,
		Status: model.StatusSuccess,
	}
	err := store.CreatePipeline(&pipeline)
	assert.Error(t, err)

	count, err := store.GetPipelineCount()
	assert.NoError(t, err)
	assert.Zero(t, count)

	// add pipeline
	pipeline = model.Pipeline{
		RepoID: repo.ID,
		Status: model.StatusSuccess,
		Commit: "85f8c029b902ed9400bc600bac301a0aadb144ac",
		Event:  model.EventPush,
		Branch: "some-branch",
	}
	err = store.CreatePipeline(&pipeline)
	assert.NoError(t, err)
	assert.NotZero(t, pipeline.ID)
	assert.EqualValues(t, 1, pipeline.Number)
	assert.Equal(t, "85f8c029b902ed9400bc600bac301a0aadb144ac", pipeline.Commit)

	count, err = store.GetPipelineCount()
	assert.NoError(t, err)
	assert.NotZero(t, count)

	GetPipeline, err := store.GetPipeline(pipeline.ID)
	assert.NoError(t, err)
	assert.Equal(t, pipeline.ID, GetPipeline.ID)
	assert.Equal(t, pipeline.RepoID, GetPipeline.RepoID)
	assert.Equal(t, pipeline.Status, GetPipeline.Status)

	// update pipeline
	pipeline.Status = model.StatusRunning
	require.NoError(t, store.UpdatePipeline(&pipeline))
	GetPipeline, err1 := store.GetPipeline(pipeline.ID)
	require.NoError(t, err1)
	assert.Equal(t, pipeline.ID, GetPipeline.ID)
	assert.Equal(t, pipeline.RepoID, GetPipeline.RepoID)
	assert.Equal(t, pipeline.Status, GetPipeline.Status)
	assert.Equal(t, pipeline.Number, GetPipeline.Number)

	pipeline2 := &model.Pipeline{
		RepoID: repo.ID,
		Status: model.StatusPending,
		Event:  model.EventPush,
		Branch: "main",
	}
	require.NoError(t, store.CreatePipeline(pipeline2, []*model.Step{}...))
	GetPipeline, err3 := store.GetPipelineNumber(&model.Repo{ID: 1}, pipeline2.Number)
	require.NoError(t, err3)
	assert.Equal(t, pipeline2.ID, GetPipeline.ID)
	assert.Equal(t, pipeline2.RepoID, GetPipeline.RepoID)
	assert.Equal(t, pipeline2.Number, GetPipeline.Number)

	GetPipeline, err4 := store.GetPipelineLastByBranch(&model.Repo{ID: repo.ID}, pipeline2.Branch)
	require.NoError(t, err4)
	assert.Equal(t, pipeline2.ID, GetPipeline.ID)
	assert.Equal(t, pipeline2.RepoID, GetPipeline.RepoID)
	assert.Equal(t, pipeline2.Number, GetPipeline.Number)
	assert.Equal(t, pipeline2.Status, GetPipeline.Status)

	pipeline3 := &model.Pipeline{
		RepoID:   repo.ID,
		Status:   model.StatusRunning,
		Branch:   "main",
		Event:    model.EventPull,
		Commit:   "85f8c029b902ed9400bc600bac301a0aadb144aa",
		ForgeURL: "example.com/id3",
	}
	require.NoError(t, store.CreatePipeline(pipeline3))

	GetPipeline, err5 := store.GetPipelineLastBefore(&model.Repo{ID: 1}, pipeline3.Branch, pipeline3.ID)
	require.NoError(t, err5)
	assert.EqualValues(t, pipeline2, GetPipeline)
}

func TestPipelineListFilter(t *testing.T) {
	repo := &model.Repo{
		UserID:   1,
		FullName: "bradrydzewski/test",
		Owner:    "bradrydzewski",
		Name:     "test",
	}

	store, closer := newTestStore(t, new(model.Repo), new(model.Step), new(model.Pipeline))
	defer closer()

	assert.NoError(t, store.CreateRepo(repo))

	pipeline1 := &model.Pipeline{
		RepoID: repo.ID,
		Status: model.StatusFailure,
		Event:  model.EventCron,
		Ref:    "refs/heads/some-branch",
		Branch: "some-branch",
	}
	pipeline2 := &model.Pipeline{
		RepoID: repo.ID,
		Status: model.StatusSuccess,
		Event:  model.EventPull,
		Ref:    "refs/pull/32",
		Branch: "main",
	}
	err := store.CreatePipeline(pipeline1, []*model.Step{}...)
	assert.NoError(t, err)
	time.Sleep(1 * time.Second)
	before := time.Now().Unix()
	err = store.CreatePipeline(pipeline2, []*model.Step{}...)
	assert.NoError(t, err)

	pipelines, err := store.GetPipelineList(&model.Repo{ID: 1}, &model.ListOptions{Page: 1, PerPage: 50}, nil)
	assert.NoError(t, err)
	assert.Len(t, (pipelines), 2)
	assert.Equal(t, pipeline2.ID, pipelines[0].ID)
	assert.Equal(t, pipeline2.RepoID, pipelines[0].RepoID)
	assert.Equal(t, pipeline2.Status, pipelines[0].Status)

	pipelines, err = store.GetPipelineList(&model.Repo{ID: 1}, nil, &model.PipelineFilter{
		Branch: "main",
	})
	assert.NoError(t, err)
	assert.Len(t, pipelines, 1)
	assert.Equal(t, pipeline2.ID, pipelines[0].ID)

	pipelines, err = store.GetPipelineList(&model.Repo{ID: 1}, nil, &model.PipelineFilter{
		Events: []model.WebhookEvent{model.EventCron},
	})
	assert.NoError(t, err)
	assert.Len(t, pipelines, 1)
	assert.Equal(t, pipeline1.ID, pipelines[0].ID)

	pipelines, err = store.GetPipelineList(&model.Repo{ID: 1}, nil, &model.PipelineFilter{
		Events:      []model.WebhookEvent{model.EventCron, model.EventPull},
		RefContains: "32",
	})
	assert.NoError(t, err)
	assert.Len(t, (pipelines), 1)
	assert.Equal(t, pipeline2.ID, pipelines[0].ID)

	pipelines, err3 := store.GetPipelineList(&model.Repo{ID: 1}, &model.ListOptions{Page: 1, PerPage: 50}, &model.PipelineFilter{Before: before})
	assert.NoError(t, err3)
	assert.Len(t, pipelines, 1)
	assert.Equal(t, pipeline1.ID, pipelines[0].ID)
	assert.Equal(t, pipeline1.RepoID, pipelines[0].RepoID)

	pipelines, err = store.GetPipelineList(&model.Repo{ID: 1}, nil, &model.PipelineFilter{
		Status: model.StatusSuccess,
	})
	assert.NoError(t, err)
	assert.Len(t, pipelines, 1)
	assert.Equal(t, pipeline2.ID, pipelines[0].ID)
	assert.Equal(t, model.StatusSuccess, pipelines[0].Status)
}

func TestPipelineIncrement(t *testing.T) {
	store, closer := newTestStore(t, new(model.Pipeline), new(model.Repo))
	defer closer()

	assert.NoError(t, store.CreateRepo(&model.Repo{ID: 1, Owner: "1", Name: "1", FullName: "1/1", ForgeRemoteID: "1"}))
	assert.NoError(t, store.CreateRepo(&model.Repo{ID: 2, Owner: "2", Name: "2", FullName: "2/2", ForgeRemoteID: "2"}))

	pipelineA := &model.Pipeline{RepoID: 1}
	require.NoError(t, store.CreatePipeline(pipelineA))
	assert.EqualValues(t, 1, pipelineA.Number)

	pipelineB := &model.Pipeline{RepoID: 1}
	assert.NoError(t, store.CreatePipeline(pipelineB))
	assert.EqualValues(t, 2, pipelineB.Number)

	pipelineC := &model.Pipeline{RepoID: 2}
	assert.NoError(t, store.CreatePipeline(pipelineC))
	assert.EqualValues(t, 1, pipelineC.Number)
}

func TestDeletePipeline(t *testing.T) {
	store, closer := newTestStore(t, new(model.Pipeline), new(model.Repo), new(model.Workflow),
		new(model.Step), new(model.LogEntry), new(model.PipelineConfig), new(model.Config))
	defer closer()

	err := wrapInsert(store.engine.Insert(
		&model.Pipeline{
			ID:     2,
			Number: 2,
			RepoID: 7,
		},
		&model.Pipeline{
			ID:     5,
			Number: 3,
			RepoID: 7,
		},
		&model.Pipeline{
			ID:     8,
			Number: 4,
			RepoID: 7,
		},
		&model.Config{
			ID:     23,
			Hash:   "1234",
			Name:   "test",
			RepoID: 7,
		},
		&model.Config{
			ID:     25,
			Hash:   "6789",
			Name:   "test",
			RepoID: 7,
		},
		&model.PipelineConfig{
			PipelineID: 2,
			ConfigID:   23,
		},
		&model.PipelineConfig{
			PipelineID: 5,
			ConfigID:   23,
		},
		&model.PipelineConfig{
			PipelineID: 8,
			ConfigID:   25,
		},
	))
	assert.NoError(t, err)

	// delete non existing pipeline
	assert.ErrorIs(t, types.ErrRecordNotExist, store.DeletePipeline(&model.Pipeline{ID: 1}))

	// delete pipeline with shares config
	assert.NoError(t, store.DeletePipeline(&model.Pipeline{ID: 2}))
	count, err := store.engine.Count(new(model.Config))
	assert.NoError(t, err)
	assert.EqualValues(t, 2, count)

	// delete pipeline with unique config
	assert.NoError(t, store.DeletePipeline(&model.Pipeline{ID: 8}))
	count, err = store.engine.Count(new(model.Config))
	assert.NoError(t, err)
	assert.EqualValues(t, 1, count)
}

// TestGetPipelineList_CorruptJSONColumn verifies that a single row with a
// corrupt JSON column (e.g. plain text in `errors`) does not poison the entire
// listing with a 500. The bad row is skipped; valid rows are returned.
// Regression for fork#38 / incident 2026-04-27 (peregrine-ci-infrastructure#1221).
func TestGetPipelineList_CorruptJSONColumn(t *testing.T) {
	repo := &model.Repo{
		UserID:   1,
		FullName: "owner/repo",
		Owner:    "owner",
		Name:     "repo",
	}

	store, closer := newTestStore(t, new(model.Repo), new(model.Pipeline))
	defer closer()

	require.NoError(t, store.CreateRepo(repo))

	good := &model.Pipeline{RepoID: repo.ID, Status: model.StatusSuccess, Event: model.EventPush, Branch: "main"}
	require.NoError(t, store.CreatePipeline(good))

	bad := &model.Pipeline{RepoID: repo.ID, Status: model.StatusError, Event: model.EventPush, Branch: "main"}
	require.NoError(t, store.CreatePipeline(bad))

	// Corrupt the `errors` column of the bad row with plain text — the exact
	// scenario from the 2026-04-27 incident where operator SQL wrote
	// "forced terminal: <msg>" into a JSON column.
	_, err := store.engine.Exec(
		"UPDATE pipelines SET errors = ? WHERE id = ?",
		"forced terminal: not valid json", bad.ID,
	)
	require.NoError(t, err)

	pipelines, err := store.GetPipelineList(repo, &model.ListOptions{Page: 1, PerPage: 50}, nil)
	assert.NoError(t, err, "listing must not fail due to single corrupt row")
	assert.Len(t, pipelines, 1, "corrupt row skipped; good row returned")
	assert.Equal(t, good.ID, pipelines[0].ID)
}

// childRowCounts returns the number of surviving child rows for a pipeline, in
// the order deletePipeline removes them: logs, steps, workflows, pipeline_config,
// config.
func childRowCounts(t *testing.T, store *storage, pipelineID, stepID, configID int64) (logs, steps, workflows, pipelineConfigs, configs int64) {
	t.Helper()
	var err error
	logs, err = store.engine.Where("step_id = ?", stepID).Count(new(model.LogEntry))
	assert.NoError(t, err)
	steps, err = store.engine.Where("pipeline_id = ?", pipelineID).Count(new(model.Step))
	assert.NoError(t, err)
	workflows, err = store.engine.Where("pipeline_id = ?", pipelineID).Count(new(model.Workflow))
	assert.NoError(t, err)
	pipelineConfigs, err = store.engine.Where("pipeline_id = ?", pipelineID).Count(new(model.PipelineConfig))
	assert.NoError(t, err)
	configs, err = store.engine.Where("id = ?", configID).Count(new(model.Config))
	assert.NoError(t, err)
	return logs, steps, workflows, pipelineConfigs, configs
}

// seedPipelineWithChildren inserts one pipeline's worth of the full cascade:
// a config it solely owns, its pipeline_config link, a workflow, a step, and a
// log entry for that step. Passing pipelineRowExists=false seeds every child
// but omits the pipeline row itself.
func seedPipelineWithChildren(t *testing.T, store *storage, pipelineID, stepID, configID int64, pipelineRowExists bool) {
	t.Helper()
	rows := []any{
		&model.Config{ID: configID, Hash: "hash", Name: "test", RepoID: 7},
		&model.PipelineConfig{PipelineID: pipelineID, ConfigID: configID},
		&model.Workflow{PipelineID: pipelineID, PID: 1, Name: "workflow"},
		&model.Step{ID: stepID, PipelineID: pipelineID, PID: 1, Name: "step", UUID: "uuid"},
		&model.LogEntry{StepID: stepID, Line: 1, Data: []byte("log line")},
	}
	if pipelineRowExists {
		rows = append(rows, &model.Pipeline{ID: pipelineID, Number: pipelineID, RepoID: 7})
	}
	assert.NoError(t, wrapInsert(store.engine.Insert(rows...)))
}

// TestDeletePipelineRollsBackOnFailure is the negative half of the #365 pair.
//
// deletePipeline removes logs, steps, workflows, config and pipeline_config
// BEFORE the pipeline row. With no transaction around it every statement
// autocommits, so a failure at the final step leaves the children destroyed and
// the pipeline row standing — and that row still returns 200, still counts as
// repo history, and reads as ordinary. A child-directional orphan check (no
// child without a parent) is clean against it, because the damage is the
// reverse shape: a parent with no children.
//
// The failure is produced without fault injection: a pipeline whose children
// exist but whose own row does not makes the final delete affect zero rows, and
// wrapDelete turns that into ErrRecordNotExist — after every child delete has
// already run. That state is reachable in production from a concurrent delete or
// from an earlier partial cascade.
func TestDeletePipelineRollsBackOnFailure(t *testing.T) {
	store, closer := newTestStore(t, new(model.Pipeline), new(model.Repo), new(model.Workflow),
		new(model.Step), new(model.LogEntry), new(model.PipelineConfig), new(model.Config))
	defer closer()

	const pipelineID, stepID, configID = 42, 4242, 424242
	seedPipelineWithChildren(t, store, pipelineID, stepID, configID, false)

	// Sanity: the children this test asserts on really are there to begin with,
	// so a later all-zero reading cannot be mistaken for a passing rollback.
	logs, steps, workflows, pipelineConfigs, configs := childRowCounts(t, store, pipelineID, stepID, configID)
	assert.Equal(t, int64(1), logs, "precondition: log seeded")
	assert.Equal(t, int64(1), steps, "precondition: step seeded")
	assert.Equal(t, int64(1), workflows, "precondition: workflow seeded")
	assert.Equal(t, int64(1), pipelineConfigs, "precondition: pipeline_config seeded")
	assert.Equal(t, int64(1), configs, "precondition: config seeded")

	// The delete must fail, because the pipeline row does not exist.
	assert.ErrorIs(t, store.DeletePipeline(&model.Pipeline{ID: pipelineID}), types.ErrRecordNotExist)

	// ...and having failed, it must have deleted nothing at all.
	logs, steps, workflows, pipelineConfigs, configs = childRowCounts(t, store, pipelineID, stepID, configID)
	assert.Equal(t, int64(1), logs, "failed delete must not destroy logs (#365)")
	assert.Equal(t, int64(1), steps, "failed delete must not destroy steps (#365)")
	assert.Equal(t, int64(1), workflows, "failed delete must not destroy workflows (#365)")
	assert.Equal(t, int64(1), pipelineConfigs, "failed delete must not destroy pipeline_config (#365)")
	assert.Equal(t, int64(1), configs, "failed delete must not destroy config (#365)")
}

// TestDeletePipelineDeletesAllChildren is the positive half of the #365 pair.
// Without it, the rollback test above could pass on a delete that never touches
// a child row at all — the exact silent-OK that a negative-only assertion hides.
func TestDeletePipelineDeletesAllChildren(t *testing.T) {
	store, closer := newTestStore(t, new(model.Pipeline), new(model.Repo), new(model.Workflow),
		new(model.Step), new(model.LogEntry), new(model.PipelineConfig), new(model.Config))
	defer closer()

	const pipelineID, stepID, configID = 43, 4343, 434343
	seedPipelineWithChildren(t, store, pipelineID, stepID, configID, true)

	assert.NoError(t, store.DeletePipeline(&model.Pipeline{ID: pipelineID}))

	logs, steps, workflows, pipelineConfigs, configs := childRowCounts(t, store, pipelineID, stepID, configID)
	assert.Equal(t, int64(0), logs, "successful delete must remove logs")
	assert.Equal(t, int64(0), steps, "successful delete must remove steps")
	assert.Equal(t, int64(0), workflows, "successful delete must remove workflows")
	assert.Equal(t, int64(0), pipelineConfigs, "successful delete must remove pipeline_config")
	assert.Equal(t, int64(0), configs, "successful delete must remove the solely-owned config")

	pipelines, err := store.engine.Where("id = ?", pipelineID).Count(new(model.Pipeline))
	assert.NoError(t, err)
	assert.Equal(t, int64(0), pipelines, "successful delete must remove the pipeline row")
}

func TestGetPipelineBadge(t *testing.T) {
	store, closer := newTestStore(t, new(model.Pipeline), new(model.Repo))
	defer closer()

	repo := &model.Repo{ID: 7}
	assert.NoError(t, wrapInsert(store.engine.Insert(
		&model.Pipeline{ID: 1, Number: 1, RepoID: 7, Branch: "main", Event: model.EventPush, Status: model.StatusSuccess},
		&model.Pipeline{ID: 2, Number: 2, RepoID: 7, Branch: "main", Event: model.EventPush, Status: model.StatusFailure},
		// higher number but blocked — must be skipped, else a badge reports the
		// state of a pipeline nobody has approved yet
		&model.Pipeline{ID: 3, Number: 3, RepoID: 7, Branch: "main", Event: model.EventPush, Status: model.StatusBlocked},
		// right branch, wrong event
		&model.Pipeline{ID: 4, Number: 4, RepoID: 7, Branch: "main", Event: model.EventCron, Status: model.StatusSuccess},
		// wrong branch
		&model.Pipeline{ID: 5, Number: 5, RepoID: 7, Branch: "other", Event: model.EventPush, Status: model.StatusSuccess},
		// wrong repo
		&model.Pipeline{ID: 6, Number: 6, RepoID: 8, Branch: "main", Event: model.EventPush, Status: model.StatusSuccess},
	)))

	pipeline, err := store.GetPipelineBadge(repo, "main", []model.WebhookEvent{model.EventPush})
	assert.NoError(t, err)
	assert.Equal(t, int64(2), pipeline.Number, "highest non-blocked push on the branch")

	// no match is a not-exist error, not an empty success
	_, err = store.GetPipelineBadge(repo, "nonexistent", []model.WebhookEvent{model.EventPush})
	assert.ErrorIs(t, err, types.ErrRecordNotExist)
}

func TestGetRepoLatestPipelines(t *testing.T) {
	store, closer := newTestStore(t, new(model.Pipeline), new(model.Repo))
	defer closer()

	assert.NoError(t, wrapInsert(store.engine.Insert(
		&model.Pipeline{ID: 1, Number: 1, RepoID: 7},
		&model.Pipeline{ID: 2, Number: 2, RepoID: 7},
		&model.Pipeline{ID: 3, Number: 1, RepoID: 8},
		// repo 9 is deliberately not requested below
		&model.Pipeline{ID: 4, Number: 1, RepoID: 9},
	)))

	pipelines, err := store.GetRepoLatestPipelines([]int64{7, 8})
	assert.NoError(t, err)
	assert.Len(t, pipelines, 2, "one latest pipeline per requested repo")

	latestByRepo := map[int64]int64{}
	for _, p := range pipelines {
		latestByRepo[p.RepoID] = p.ID
	}
	assert.Equal(t, int64(2), latestByRepo[7], "MAX(id) for repo 7")
	assert.Equal(t, int64(3), latestByRepo[8], "MAX(id) for repo 8")
	assert.NotContains(t, latestByRepo, int64(9), "unrequested repo must not leak in")

	// an empty request must not return every repo's latest
	pipelines, err = store.GetRepoLatestPipelines([]int64{})
	assert.NoError(t, err)
	assert.Empty(t, pipelines)
}

func TestGetActivePipelineList(t *testing.T) {
	store, closer := newTestStore(t, new(model.Pipeline), new(model.Repo))
	defer closer()

	repo := &model.Repo{ID: 7}
	assert.NoError(t, wrapInsert(store.engine.Insert(
		&model.Pipeline{ID: 1, Number: 1, RepoID: 7, Status: model.StatusPending},
		&model.Pipeline{ID: 2, Number: 2, RepoID: 7, Status: model.StatusRunning},
		&model.Pipeline{ID: 3, Number: 3, RepoID: 7, Status: model.StatusBlocked},
		// terminal states must not be reported as active
		&model.Pipeline{ID: 4, Number: 4, RepoID: 7, Status: model.StatusSuccess},
		&model.Pipeline{ID: 5, Number: 5, RepoID: 7, Status: model.StatusFailure},
		&model.Pipeline{ID: 6, Number: 6, RepoID: 7, Status: model.StatusKilled},
		&model.Pipeline{ID: 7, Number: 7, RepoID: 7, Status: model.StatusError},
		// another repo's active pipeline must not leak in
		&model.Pipeline{ID: 8, Number: 1, RepoID: 8, Status: model.StatusRunning},
	)))

	pipelines, err := store.GetActivePipelineList(repo)
	assert.NoError(t, err)
	assert.Len(t, pipelines, 3)

	// descending by number
	assert.Equal(t, int64(3), pipelines[0].Number)
	assert.Equal(t, int64(2), pipelines[1].Number)
	assert.Equal(t, int64(1), pipelines[2].Number)

	// and a repo with no active pipelines reports none rather than erroring
	pipelines, err = store.GetActivePipelineList(&model.Repo{ID: 999})
	assert.NoError(t, err)
	assert.Empty(t, pipelines)
}

func TestIsUniqueConstraintError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil error is not a constraint violation", err: nil, want: false},
		{name: "unrelated error", err: errors.New("connection refused"), want: false},
		// the five dialect spellings the function claims to recognise, one case
		// each — a table rather than one representative, so dropping a branch
		// fails loudly instead of staying green on the survivors
		{name: "postgres", err: errors.New(`pq: duplicate key value violates unique constraint "pipelines_pkey"`), want: true},
		{name: "mysql", err: errors.New("Error 1062: Duplicate entry '7-1' for key 'pipeline_number'"), want: true},
		{name: "sqlite", err: errors.New("UNIQUE constraint failed: pipelines.repo_id, pipelines.number"), want: true},
		{name: "lowercase unique constraint", err: errors.New("violates unique constraint"), want: true},
		{name: "unique violation", err: errors.New("UNIQUE violation on pipelines"), want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isUniqueConstraintError(tt.err))
		})
	}
}

// TestCreatePipelineUniqueConstraintRetry covers the retry arm of
// CreatePipeline. A unique-constraint failure is returned bare so backoff
// retries it (a concurrent creator may have taken the number we computed);
// anything else is wrapped in backoff.Permanent so it fails immediately. These
// two must stay distinguishable — collapsing them would either retry a
// permanent error three times or give up on a recoverable collision.
func TestCreatePipelineUniqueConstraintRetry(t *testing.T) {
	store, closer := newTestStore(t, new(model.Pipeline), new(model.Repo), new(model.Step))
	defer closer()

	assert.NoError(t, wrapInsert(store.engine.Insert(&model.Repo{ID: 7, IsActive: true})))

	t.Run("pipeline insert collision retries and then reports", func(t *testing.T) {
		assert.NoError(t, wrapInsert(store.engine.Insert(&model.Pipeline{ID: 900, Number: 900, RepoID: 7})))

		// explicit ID collides with the row above on the primary key
		err := store.CreatePipeline(&model.Pipeline{ID: 900, RepoID: 7}, []*model.Step{}...)
		assert.Error(t, err, "a colliding insert must surface, not be swallowed")
	})

	t.Run("step insert collision surfaces", func(t *testing.T) {
		// two steps sharing a PID collide on Step's UNIQUE(pipeline_id, pid)
		err := store.CreatePipeline(&model.Pipeline{RepoID: 7},
			&model.Step{PID: 1, Name: "a", UUID: "uuid-a"},
			&model.Step{PID: 1, Name: "b", UUID: "uuid-b"},
		)
		assert.Error(t, err, "a colliding step insert must surface, not be swallowed")
	})

	t.Run("missing repo is a typed error, not a retry", func(t *testing.T) {
		err := store.CreatePipeline(&model.Pipeline{RepoID: 4242}, []*model.Step{}...)
		assert.ErrorAs(t, err, &ErrorRepoNotExist{}, "a nonexistent repo must be typed, not retried as a collision")
	})
}

// TestGetPipelineListFilterAfterAndStatuses covers the two PipelineFilter arms
// that no existing test exercised: After (the lower time bound) and Statuses
// (the multi-status filter, #881, which takes precedence over the single-status
// Status field it superseded). Both are real behaviour rather than error wraps,
// so an untested arm silently returns the wrong rows rather than failing.
func TestGetPipelineListFilterAfterAndStatuses(t *testing.T) {
	store, closer := newTestStore(t, new(model.Pipeline), new(model.Repo))
	defer closer()

	repo := &model.Repo{ID: 7}
	assert.NoError(t, wrapInsert(store.engine.Insert(
		&model.Pipeline{ID: 1, Number: 1, RepoID: 7, Status: model.StatusSuccess},
		&model.Pipeline{ID: 2, Number: 2, RepoID: 7, Status: model.StatusFailure},
		&model.Pipeline{ID: 3, Number: 3, RepoID: 7, Status: model.StatusError},
	)))

	// Pipeline.Created carries xorm's `created` tag, so xorm overwrites any value
	// passed to Insert with time.Now(). Set the column directly, or every row
	// lands at "now" and a time-bound filter silently matches all of them.
	for id, created := range map[int64]int64{1: 100, 2: 200, 3: 300} {
		_, err := store.engine.Exec("UPDATE pipelines SET created = ? WHERE id = ?", created, id)
		assert.NoError(t, err)
	}

	t.Run("After is an exclusive lower bound on created", func(t *testing.T) {
		pipelines, err := store.GetPipelineList(repo, &model.ListOptions{All: true}, &model.PipelineFilter{After: 150})
		assert.NoError(t, err)
		assert.Len(t, pipelines, 2, "created=100 is excluded, 200 and 300 kept")

		// boundary: After equal to a row's created excludes that row (Gt, not Gte)
		pipelines, err = store.GetPipelineList(repo, &model.ListOptions{All: true}, &model.PipelineFilter{After: 200})
		assert.NoError(t, err)
		assert.Len(t, pipelines, 1, "created=200 excluded at the boundary, only 300 kept")
	})

	t.Run("Statuses matches any of several", func(t *testing.T) {
		pipelines, err := store.GetPipelineList(repo, &model.ListOptions{All: true},
			&model.PipelineFilter{Statuses: []model.StatusValue{model.StatusFailure, model.StatusError}})
		assert.NoError(t, err)
		assert.Len(t, pipelines, 2)
		for _, p := range pipelines {
			assert.NotEqual(t, model.StatusSuccess, p.Status)
		}
	})

	t.Run("Statuses takes precedence over the single Status it superseded", func(t *testing.T) {
		// if the else-if arm were reachable here, this would return 1 row
		pipelines, err := store.GetPipelineList(repo, &model.ListOptions{All: true},
			&model.PipelineFilter{Statuses: []model.StatusValue{model.StatusFailure, model.StatusError}, Status: model.StatusSuccess})
		assert.NoError(t, err)
		assert.Len(t, pipelines, 2, "Statuses wins; the Status=success filter must be ignored")
	})
}
