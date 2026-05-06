// Copyright 2021 Woodpecker Authors
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
	"context"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v5"
	"github.com/rs/zerolog/log"
	"xorm.io/builder"
	"xorm.io/xorm"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
)

func (s storage) GetPipeline(id int64) (*model.Pipeline, error) {
	pipeline := &model.Pipeline{}
	return pipeline, wrapGet(s.engine.ID(id).Get(pipeline))
}

func (s storage) GetPipelineNumber(repo *model.Repo, num int64) (*model.Pipeline, error) {
	pipeline := new(model.Pipeline)
	return pipeline, wrapGet(s.engine.Where(
		builder.Eq{"repo_id": repo.ID, "number": num},
	).Get(pipeline))
}

func (s storage) GetPipelineBadge(repo *model.Repo, branch string, events []model.WebhookEvent) (*model.Pipeline, error) {
	pipeline := new(model.Pipeline)
	return pipeline, wrapGet(s.engine.
		Desc("number").
		Where(builder.Eq{"repo_id": repo.ID, "branch": branch}).
		Where(builder.In("event", events)).
		Where(builder.Neq{"status": model.StatusBlocked}).
		Get(pipeline))
}

func (s storage) GetPipelineLastByBranch(repo *model.Repo, branch string) (*model.Pipeline, error) {
	pipeline := new(model.Pipeline)
	return pipeline, wrapGet(s.engine.
		Desc("number").
		Where(builder.Eq{"repo_id": repo.ID, "branch": branch, "event": model.EventPush}).
		Get(pipeline))
}

func (s storage) GetPipelineLastBefore(repo *model.Repo, branch string, num int64) (*model.Pipeline, error) {
	pipeline := new(model.Pipeline)
	return pipeline, wrapGet(s.engine.
		Desc("number").
		Where(builder.Lt{"id": num}.
			And(builder.Eq{"repo_id": repo.ID, "branch": branch})).
		Get(pipeline))
}

func (s storage) GetPipelineList(repo *model.Repo, p *model.ListOptions, f *model.PipelineFilter) ([]*model.Pipeline, error) {
	cond := builder.NewCond().And(builder.Eq{"repo_id": repo.ID})

	if f != nil {
		if f.After != 0 {
			cond = cond.And(builder.Gt{"created": f.After})
		}

		if f.Before != 0 {
			cond = cond.And(builder.Lt{"created": f.Before})
		}

		if f.Branch != "" {
			cond = cond.And(builder.Eq{"branch": f.Branch})
		}

		if len(f.Statuses) > 0 {
			cond = cond.And(builder.In("status", f.Statuses))
		} else if f.Status != "" {
			cond = cond.And(builder.Eq{"status": f.Status})
		}

		if len(f.Events) != 0 {
			cond = cond.And(builder.In("event", f.Events))
		}

		if f.RefContains != "" {
			cond = cond.And(builder.Like{"ref", f.RefContains})
		}
	}

	return scanPipelines(s.paginate(p).Where(cond).Desc("number"))
}

// GetRepoLatestPipelines get the latest pipeline for each repo.
func (s storage) GetRepoLatestPipelines(repoIDs []int64) ([]*model.Pipeline, error) {
	pipelineIDs := make([]int64, 0, len(repoIDs))
	if err := s.engine.Select("MAX(id) AS id").
		Table("pipelines").
		Where(builder.In("repo_id", repoIDs)).
		GroupBy("repo_id").
		Find(&pipelineIDs); err != nil {
		return nil, err
	}

	return scanPipelines(s.engine.Where(builder.In("id", pipelineIDs)))
}

// GetActivePipelineList get all pipelines that are pending, running or blocked.
func (s storage) GetActivePipelineList(repo *model.Repo) ([]*model.Pipeline, error) {
	return scanPipelines(s.engine.
		Where("repo_id = ?", repo.ID).
		In("status", model.StatusPending, model.StatusRunning, model.StatusBlocked).
		Desc("number"))
}

// scanPipelines iterates rows from the given xorm session using a cursor so
// that a single row with a corrupt JSON column (e.g. errors, cancel_info) is
// skipped and logged rather than failing the entire listing with HTTP 500.
// Incident 2026-04-27: one bad row in `errors` caused 15 hours of 500s across
// 13 repos (peregrine-ci-infrastructure#1221, fork#38).
func scanPipelines(sess *xorm.Session) ([]*model.Pipeline, error) {
	rows, err := sess.Rows(new(model.Pipeline))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var pipelines []*model.Pipeline
	for rows.Next() {
		p := new(model.Pipeline)
		if err := rows.Scan(p); err != nil {
			log.Warn().Err(err).Msg("pipeline listing: skipping row with corrupt JSON column")
			continue
		}
		pipelines = append(pipelines, p)
	}
	return pipelines, rows.Err()
}

func (s storage) GetPipelineCount() (int64, error) {
	return s.engine.Count(new(model.Pipeline))
}

// CreatePipeline creates a new pipeline with retry logic for unique constraint errors.
func (s storage) CreatePipeline(pipeline *model.Pipeline, stepList ...*model.Step) error {
	return s.wq.serialize(func() error {
		// Maximum number of retries
		const maxRetries = 3

		// Create backoff configuration
		exponentialBackoff := backoff.NewExponentialBackOff()

		// Execute with backoff retry
		_, err := backoff.Retry(context.Background(), func() (struct{}, error) {
			sess := s.writeEngine().NewSession()
			defer sess.Close()
			if err := sess.Begin(); err != nil {
				return struct{}{}, err
			}

			repoExist, err := sess.Where("id = ?", pipeline.RepoID).Exist(&model.Repo{})
			if err != nil {
				return struct{}{}, err
			}

			if !repoExist {
				return struct{}{}, ErrorRepoNotExist{RepoID: pipeline.RepoID}
			}

			// calc pipeline number
			var number int64
			if _, err := sess.Select("MAX(number)").
				Table(new(model.Pipeline)).
				Where("repo_id = ?", pipeline.RepoID).
				Get(&number); err != nil {
				return struct{}{}, err
			}
			pipeline.Number = number + 1

			pipeline.Created = time.Now().UTC().Unix()
			// only Insert set auto created ID back to object
			if err := wrapInsert(sess.Insert(pipeline)); err != nil {
				if isUniqueConstraintError(err) {
					return struct{}{}, err
				}
				return struct{}{}, backoff.Permanent(err)
			}

			for i := range stepList {
				stepList[i].PipelineID = pipeline.ID
				// only Insert set auto created ID back to object
				if err := wrapInsert(sess.Insert(stepList[i])); err != nil {
					if isUniqueConstraintError(err) {
						return struct{}{}, err
					}
					return struct{}{}, backoff.Permanent(err)
				}
			}

			return struct{}{}, sess.Commit()
		}, backoff.WithBackOff(exponentialBackoff), backoff.WithMaxTries(maxRetries))

		return err
	})
}

// isUniqueConstraintError checks if an error is a unique constraint violation error.
func isUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}

	errStr := err.Error()
	// Check for common unique constraint error patterns across different databases
	return strings.Contains(errStr, "duplicate key") ||
		strings.Contains(errStr, "Duplicate entry") ||
		strings.Contains(errStr, "UNIQUE constraint failed") ||
		strings.Contains(errStr, "unique constraint") ||
		strings.Contains(errStr, "UNIQUE violation")
}

func (s storage) UpdatePipeline(pipeline *model.Pipeline) error {
	return s.wq.serialize(func() error {
		_, err := s.writeEngine().ID(pipeline.ID).AllCols().Update(pipeline)
		return err
	})
}

func (s storage) DeletePipeline(pipeline *model.Pipeline) error {
	return s.wq.serialize(func() error {
		return s.deletePipeline(s.writeEngine().NewSession(), pipeline.ID)
	})
}

func (s storage) deletePipeline(sess *xorm.Session, pipelineID int64) error {
	if err := s.workflowsDelete(sess, pipelineID); err != nil {
		return err
	}

	var confIDs []int64
	if err := sess.Table(new(model.PipelineConfig)).Select("config_id").Where("pipeline_id = ?", pipelineID).Find(&confIDs); err != nil {
		return err
	}
	for _, confID := range confIDs {
		exist, err := sess.Where(builder.Eq{"config_id": confID}.And(builder.Neq{"pipeline_id": pipelineID})).Exist(new(model.PipelineConfig))
		if err != nil {
			return err
		}
		if !exist {
			// this config is only used for this pipeline. so delete it
			if _, err := sess.Where(builder.Eq{"id": confID}).Delete(new(model.Config)); err != nil {
				return err
			}
		}
	}

	if _, err := sess.Where("pipeline_id = ?", pipelineID).Delete(new(model.PipelineConfig)); err != nil {
		return err
	}
	return wrapDelete(sess.ID(pipelineID).Delete(new(model.Pipeline)))
}
