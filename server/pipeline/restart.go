// Copyright 2022 Woodpecker Authors
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

package pipeline

import (
	"context"
	"errors"
	"fmt"

	"github.com/rs/zerolog/log"

	"go.woodpecker-ci.org/woodpecker/v3/server"
	forge_types "go.woodpecker-ci.org/woodpecker/v3/server/forge/types"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/plugin"
	"go.woodpecker-ci.org/woodpecker/v3/server/store"
)

// Restart a pipeline by creating a new one out of the old and start it.
func Restart(ctx context.Context, store store.Store, lastPipeline *model.Pipeline, user *model.User, repo *model.Repo, envs map[string]string) (*model.Pipeline, error) {
	forge, err := server.Config.Services.Manager.ForgeFromRepo(repo)
	if err != nil {
		msg := fmt.Sprintf("failure to load forge for repo '%s'", repo.FullName)
		log.Error().Err(err).Str("repo", repo.FullName).Msg(msg)
		return nil, errors.New(msg)
	}

	if lastPipeline.Status == model.StatusBlocked {
		return nil, &ErrBadRequest{Msg: "cannot restart a pipeline with status blocked"}
	}

	// fetch the old pipeline config from the database
	configs, err := store.ConfigsForPipeline(lastPipeline.ID)
	if err != nil {
		log.Error().Err(err).Msgf("failure to get pipeline config for %s", repo.FullName)
		return nil, &ErrNotFound{Msg: fmt.Sprintf("failure to get pipeline config for %s. %s", repo.FullName, err)}
	}

	var pipelineFiles []*forge_types.FileMeta
	for _, y := range configs {
		pipelineFiles = append(pipelineFiles, &forge_types.FileMeta{Data: y.Data, Name: y.Name})
	}

	// If the config service is active we should refetch the config in case something changed
	configService := server.Config.Services.Manager.ConfigServiceFromRepo(repo)
	pipelineFiles, err = configService.Fetch(ctx, forge, user, repo, lastPipeline, pipelineFiles, true)
	if err != nil {
		return nil, &ErrBadRequest{
			Msg: fmt.Sprintf("On fetching external pipeline config: %s", err),
		}
	}

	newPipeline := createNewOutOfOld(lastPipeline)
	newPipeline.Parent = lastPipeline.Number

	err = store.CreatePipeline(newPipeline)
	if err != nil {
		msg := fmt.Sprintf("failure to save pipeline for %s", repo.FullName)
		log.Error().Err(err).Msg(msg)
		return nil, errors.New(msg)
	}

	// #36: if the old pipeline has no linked configs (observed today on
	// deployment-event pipelines that errored before steps ran — see
	// peregrine-grafana #266 → restart #267), fall back to the fresh files
	// just fetched from the forge. configService.Fetch already pulled them
	// at lastPipeline.Commit when oldConfigData was empty. Persist them as
	// new Config rows and link to the new pipeline so subsequent
	// createPipelineItems has something to work with.
	//
	// Only error out if BOTH the database link AND the forge fetch returned
	// nothing — that's the genuine "pipeline definition not found" case.
	if len(configs) == 0 && len(pipelineFiles) == 0 {
		newPipeline, uErr := UpdateToStatusError(store, *newPipeline, errors.New("pipeline definition not found"))
		if uErr != nil {
			log.Debug().Err(uErr).Msg("failure to update pipeline status")
		} else {
			EmitEvent(plugin.EventPipelineFailed, repo, newPipeline, "")
			updatePipelineStatus(ctx, forge, newPipeline, repo, user)
		}
		return newPipeline, nil
	}
	if len(configs) == 0 {
		log.Info().
			Str("repo", repo.FullName).
			Int64("pipeline_id", lastPipeline.ID).
			Str("event", string(lastPipeline.Event)).
			Int("files", len(pipelineFiles)).
			Msg("restart: no linked configs — persisting fresh files from forge (#36)")
		recovered, recErr := recoverConfigsFromForge(store, repo.ID, pipelineFiles)
		if recErr != nil {
			msg := fmt.Sprintf("failure to recover config for %s: %s", repo.FullName, recErr)
			log.Error().Err(recErr).Msg(msg)
			return nil, errors.New(msg)
		}
		configs = recovered
	}
	if err := linkPipelineConfigs(store, configs, newPipeline.ID); err != nil {
		msg := fmt.Sprintf("failure to persist pipeline config for %s.", repo.FullName)
		log.Error().Err(err).Msg(msg)
		return nil, errors.New(msg)
	}

	newPipeline, pipelineItems, err := createPipelineItems(ctx, forge, store, newPipeline, user, repo, pipelineFiles, envs)
	if err != nil {
		msg := fmt.Sprintf("failure to createPipelineItems for %s", repo.FullName)
		log.Error().Err(err).Msg(msg)
		return nil, errors.New(msg)
	}

	if err := prepareStart(ctx, forge, store, newPipeline, user, repo); err != nil {
		msg := fmt.Sprintf("failure to prepare pipeline for %s", repo.FullName)
		log.Error().Err(err).Msg(msg)
		return nil, errors.New(msg)
	}

	newPipeline, err = start(ctx, forge, store, newPipeline, user, repo, pipelineItems)
	if err != nil {
		msg := fmt.Sprintf("failure to start pipeline for %s", repo.FullName)
		log.Error().Err(err).Msg(msg)
		return nil, errors.New(msg)
	}

	return newPipeline, nil
}

func linkPipelineConfigs(store store.Store, configs []*model.Config, pipelineID int64) error {
	for _, conf := range configs {
		pipelineConfig := &model.PipelineConfig{
			ConfigID:   conf.ID,
			PipelineID: pipelineID,
		}
		err := store.PipelineConfigCreate(pipelineConfig)
		if err != nil {
			return err
		}
	}
	return nil
}

// recoverConfigsFromForge persists freshly-fetched pipeline yaml files as
// Config rows and returns them, so a restart whose database link is missing
// can still proceed with content fetched directly from the forge. Idempotent
// on content via ConfigPersist's hash-based dedup. (#36)
func recoverConfigsFromForge(store store.Store, repoID int64, files []*forge_types.FileMeta) ([]*model.Config, error) {
	out := make([]*model.Config, 0, len(files))
	for _, f := range files {
		persisted, err := store.ConfigPersist(&model.Config{RepoID: repoID, Data: f.Data, Name: f.Name})
		if err != nil {
			return nil, fmt.Errorf("persist recovered config %q: %w", f.Name, err)
		}
		out = append(out, persisted)
	}
	return out, nil
}

func createNewOutOfOld(old *model.Pipeline) *model.Pipeline {
	newPipeline := *old
	newPipeline.ID = 0
	newPipeline.Number = 0
	newPipeline.Status = model.StatusPending
	newPipeline.Started = 0
	newPipeline.Finished = 0
	newPipeline.Errors = nil
	return &newPipeline
}
