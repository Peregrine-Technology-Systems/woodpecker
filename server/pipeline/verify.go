// Copyright 2026 Peregrine Technology Systems
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
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/store"
)

// verifyHTTPClient is the client used for server-side verify proof-queries.
// Package-level so tests can point it at an httptest server.
var verifyHTTPClient = &http.Client{Timeout: 15 * time.Second}

// verifyCommitPrefixLen is the short-SHA length the deploy /version contract
// uses when reporting the running commit.
const verifyCommitPrefixLen = 7

// maxVerifyBodyBytes caps how much of a probe response we read before parsing.
const maxVerifyBodyBytes = 1 << 20

// ReconcileVerifiedKilledSteps runs the server-side outcome-verification
// proof-query for every killed step in the workflow that declares one and
// reconciles a step to success when its proof-query confirms the work actually
// landed. Returns the number of steps reconciled. (Peregrine #235, Option A)
//
// This is the recovery path for a GENUINE kill — a step that was actually
// killed mid-execution but whose underlying work nonetheless completed, e.g. an
// agent killed by a scale-down or preemption mid-deploy after the deploy had
// already reached the target. The probe is a single read-only HTTP GET; it is
// never a re-run of the step's work.
//
// It is NOT the #230/#232 path. Those are spurious cancels where no step was
// killed at all — the workflow-success invariant in UpdateWorkflowStatusToDone
// reconciles those without any probe. Composing the two: when this function
// flips the only killed step to success, that invariant then sees no killed
// child and computes the workflow as success.
func ReconcileVerifiedKilledSteps(ctx context.Context, store store.Store, workflow *model.Workflow) int {
	reconciled := 0
	for _, step := range workflow.Children {
		if step.State != model.StatusKilled || step.Verify == nil || step.Verify.URL == "" {
			continue
		}

		ok, err := runVerifyProbe(ctx, step.Verify)
		switch {
		case err != nil:
			log.Warn().Err(err).Int64("step_id", step.ID).Str("url", step.Verify.URL).
				Msg("verify: proof-query errored — confirming killed (#235)")
			continue
		case !ok:
			log.Info().Int64("step_id", step.ID).Str("url", step.Verify.URL).
				Msg("verify: proof-query did not match — confirming killed (#235)")
			continue
		}

		log.Info().Int64("step_id", step.ID).Str("url", step.Verify.URL).
			Msg("verify: proof-query confirmed work landed — reconciling killed step to success (#235)")
		step.State = model.StatusSuccess
		step.Error = ""
		step.ExitCode = 0
		if err := store.StepUpdate(step); err != nil {
			log.Error().Err(err).Int64("step_id", step.ID).
				Msg("verify: failed to persist reconciled step — leaving killed")
			step.State = model.StatusKilled // keep in-memory state honest for the caller
			continue
		}
		reconciled++
	}
	return reconciled
}

// runVerifyProbe issues the read-only HTTP GET and evaluates the expectations:
// the response status must match (default 200) and, when an expected commit is
// declared, the JSON body's `commit` field must match it by short-SHA prefix.
func runVerifyProbe(ctx context.Context, v *model.StepVerify) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.URL, nil)
	if err != nil {
		return false, err
	}

	resp, err := verifyHTTPClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	expectStatus := v.ExpectStatus
	if expectStatus == 0 {
		expectStatus = http.StatusOK
	}
	if resp.StatusCode != expectStatus {
		return false, nil
	}

	// No commit expectation → a matching status is sufficient proof.
	if v.ExpectCommit == "" {
		return true, nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxVerifyBodyBytes))
	if err != nil {
		return false, err
	}
	var payload struct {
		Commit string `json:"commit"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return false, err
	}
	return commitMatches(payload.Commit, v.ExpectCommit), nil
}

// commitMatches compares two commit SHAs by their short (7-char) prefix — the
// convention the deploy /version contract uses. Empty values never match.
func commitMatches(got, want string) bool {
	got, want = strings.TrimSpace(got), strings.TrimSpace(want)
	if got == "" || want == "" {
		return false
	}
	return shortSHA(got) == shortSHA(want)
}

func shortSHA(s string) string {
	if len(s) > verifyCommitPrefixLen {
		return s[:verifyCommitPrefixLen]
	}
	return s
}
