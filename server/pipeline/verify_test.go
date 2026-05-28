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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	store_mocks "go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
)

func TestCommitMatches(t *testing.T) {
	assert.True(t, commitMatches("abcdef0123456", "abcdef0789xyz"), "match on 7-char prefix")
	assert.True(t, commitMatches("abcdef0", "abcdef0123456"), "short got vs long want")
	assert.True(t, commitMatches(" abcdef0123 \n", "abcdef0123"), "trims whitespace")
	assert.False(t, commitMatches("abcdef0", "1234567"), "different prefixes")
	assert.False(t, commitMatches("", "abcdef0"), "empty got never matches")
	assert.False(t, commitMatches("abcdef0", ""), "empty want never matches")
}

// reconcileWith runs ReconcileVerifiedKilledSteps with the package HTTP client
// pointed at the given test handler, restoring it afterwards.
func reconcileWith(t *testing.T, handler http.HandlerFunc, step *model.Step, expectUpdate bool) int {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	orig := verifyHTTPClient
	verifyHTTPClient = srv.Client()
	t.Cleanup(func() { verifyHTTPClient = orig })

	if step.Verify != nil && step.Verify.URL == "use-test-server" {
		step.Verify.URL = srv.URL
	}

	mockStore := store_mocks.NewMockStore(t)
	if expectUpdate {
		mockStore.On("StepUpdate", mock.MatchedBy(func(s *model.Step) bool {
			return s.State == model.StatusSuccess && s.Error == "" && s.ExitCode == 0
		})).Return(nil)
	}

	wf := &model.Workflow{ID: 1, Children: []*model.Step{step}}
	return ReconcileVerifiedKilledSteps(context.Background(), mockStore, wf)
}

func TestReconcileVerifiedKilledSteps_CommitMatchReconciles(t *testing.T) {
	step := &model.Step{
		ID:    1,
		State: model.StatusKilled,
		Verify: &model.StepVerify{
			URL:          "use-test-server",
			ExpectCommit: "abcdef0123456789",
		},
	}
	n := reconcileWith(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"commit":"abcdef0","version":"1.2.3"}`))
	}, step, true)

	assert.Equal(t, 1, n)
	assert.Equal(t, model.StatusSuccess, step.State, "matching commit reconciles killed step to success")
}

func TestReconcileVerifiedKilledSteps_CommitMismatchStaysKilled(t *testing.T) {
	step := &model.Step{
		ID:     1,
		State:  model.StatusKilled,
		Verify: &model.StepVerify{URL: "use-test-server", ExpectCommit: "abcdef0"},
	}
	n := reconcileWith(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"commit":"9999999"}`))
	}, step, false)

	assert.Equal(t, 0, n)
	assert.Equal(t, model.StatusKilled, step.State, "commit mismatch leaves step killed")
}

func TestReconcileVerifiedKilledSteps_Non200StaysKilled(t *testing.T) {
	step := &model.Step{
		ID:     1,
		State:  model.StatusKilled,
		Verify: &model.StepVerify{URL: "use-test-server", ExpectCommit: "abcdef0"},
	}
	n := reconcileWith(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}, step, false)

	assert.Equal(t, 0, n)
	assert.Equal(t, model.StatusKilled, step.State)
}

func TestReconcileVerifiedKilledSteps_StatusOnlyMatch(t *testing.T) {
	// No expected commit + custom expected status → status match is sufficient.
	step := &model.Step{
		ID:     1,
		State:  model.StatusKilled,
		Verify: &model.StepVerify{URL: "use-test-server", ExpectStatus: http.StatusNoContent},
	}
	n := reconcileWith(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}, step, true)

	assert.Equal(t, 1, n)
	assert.Equal(t, model.StatusSuccess, step.State)
}

func TestReconcileVerifiedKilledSteps_InvalidJSONStaysKilled(t *testing.T) {
	step := &model.Step{
		ID:     1,
		State:  model.StatusKilled,
		Verify: &model.StepVerify{URL: "use-test-server", ExpectCommit: "abcdef0"},
	}
	n := reconcileWith(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}, step, false)

	assert.Equal(t, 0, n)
	assert.Equal(t, model.StatusKilled, step.State)
}

func TestReconcileVerifiedKilledSteps_ProbeErrorStaysKilled(t *testing.T) {
	// Point at an address that refuses connections — Do() returns an error.
	step := &model.Step{
		ID:     1,
		State:  model.StatusKilled,
		Verify: &model.StepVerify{URL: "http://127.0.0.1:0/version", ExpectCommit: "abcdef0"},
	}
	mockStore := store_mocks.NewMockStore(t)
	wf := &model.Workflow{ID: 1, Children: []*model.Step{step}}
	n := ReconcileVerifiedKilledSteps(context.Background(), mockStore, wf)

	assert.Equal(t, 0, n)
	assert.Equal(t, model.StatusKilled, step.State)
}

func TestReconcileVerifiedKilledSteps_BadURLStaysKilled(t *testing.T) {
	// A control character in the URL makes http.NewRequestWithContext fail.
	step := &model.Step{
		ID:     1,
		State:  model.StatusKilled,
		Verify: &model.StepVerify{URL: "http://exa\x7fmple/version", ExpectCommit: "abcdef0"},
	}
	mockStore := store_mocks.NewMockStore(t)
	wf := &model.Workflow{ID: 1, Children: []*model.Step{step}}
	n := ReconcileVerifiedKilledSteps(context.Background(), mockStore, wf)

	assert.Equal(t, 0, n)
	assert.Equal(t, model.StatusKilled, step.State)
}

func TestReconcileVerifiedKilledSteps_SkipsNonKilledAndNoVerify(t *testing.T) {
	// A successful step and a killed step with no verify config: neither probed.
	successStep := &model.Step{ID: 1, State: model.StatusSuccess, Verify: &model.StepVerify{URL: "x"}}
	killedNoVerify := &model.Step{ID: 2, State: model.StatusKilled}
	killedEmptyURL := &model.Step{ID: 3, State: model.StatusKilled, Verify: &model.StepVerify{}}

	mockStore := store_mocks.NewMockStore(t)
	wf := &model.Workflow{ID: 1, Children: []*model.Step{successStep, killedNoVerify, killedEmptyURL}}
	n := ReconcileVerifiedKilledSteps(context.Background(), mockStore, wf)

	assert.Equal(t, 0, n)
	assert.Equal(t, model.StatusSuccess, successStep.State)
	assert.Equal(t, model.StatusKilled, killedNoVerify.State)
	assert.Equal(t, model.StatusKilled, killedEmptyURL.State)
}

func TestReconcileVerifiedKilledSteps_StepUpdateErrorRevertsToKilled(t *testing.T) {
	step := &model.Step{
		ID:     1,
		State:  model.StatusKilled,
		Verify: &model.StepVerify{URL: "use-test-server", ExpectCommit: "abcdef0123"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"commit":"abcdef0"}`))
	}))
	t.Cleanup(srv.Close)
	step.Verify.URL = srv.URL

	orig := verifyHTTPClient
	verifyHTTPClient = srv.Client()
	t.Cleanup(func() { verifyHTTPClient = orig })

	mockStore := store_mocks.NewMockStore(t)
	mockStore.On("StepUpdate", mock.Anything).Return(assert.AnError)

	wf := &model.Workflow{ID: 1, Children: []*model.Step{step}}
	n := ReconcileVerifiedKilledSteps(context.Background(), mockStore, wf)

	assert.Equal(t, 0, n, "persist failure is not counted as reconciled")
	assert.Equal(t, model.StatusKilled, step.State, "persist failure leaves in-memory state killed")
}
