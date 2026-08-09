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
	"go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
)

// TestFinalizeKilledWorkflow_KillsRunningAndPendingSteps covers the exact
// symptom in woodpecker#349: a step left at running/pending must not stay
// that way — it needs a terminal state and a Finished timestamp.
func TestFinalizeKilledWorkflow_KillsRunningAndPendingSteps(t *testing.T) {
	running := &model.Step{ID: 1, State: model.StatusRunning, Started: 42}
	pending := &model.Step{ID: 2, State: model.StatusPending}
	done := &model.Step{ID: 3, State: model.StatusSuccess, Finished: 99}

	wf := &model.Workflow{ID: 10, Children: []*model.Step{running, pending, done}}

	s := mocks.NewMockStore(t)
	s.On("StepUpdate", mock.MatchedBy(func(st *model.Step) bool {
		return (st.ID == 1 || st.ID == 2) && st.State == model.StatusKilled &&
			st.Finished == int64(1000) && st.Error == "sig"
	})).Return(nil).Twice()
	s.On("WorkflowUpdate", mock.MatchedBy(func(w *model.Workflow) bool {
		return w.ID == 10 && w.State == model.StatusKilled && w.Error == "sig" && w.Finished == int64(1000)
	})).Return(nil)

	FinalizeKilledWorkflow(context.Background(), s, wf, 1000, "sig")

	assert.Equal(t, model.StatusKilled, running.State)
	assert.Equal(t, model.StatusKilled, pending.State)
	assert.Equal(t, model.StatusSuccess, done.State, "an already-terminal step must not be touched")
}

// TestFinalizeKilledWorkflow_AllStepsAlreadyDone mirrors rpc.go's #168
// contract: if nothing was actually in-flight, derive the workflow's state
// from its children's real outcome instead of stamping Killed.
func TestFinalizeKilledWorkflow_AllStepsAlreadyDone(t *testing.T) {
	step := &model.Step{ID: 1, State: model.StatusSuccess, Finished: 99}
	wf := &model.Workflow{ID: 10, Children: []*model.Step{step}}

	s := mocks.NewMockStore(t)
	s.On("WorkflowUpdate", mock.MatchedBy(func(w *model.Workflow) bool {
		return w.ID == 10 && w.State == model.StatusSuccess && w.Error == ""
	})).Return(nil)

	FinalizeKilledWorkflow(context.Background(), s, wf, 1000, "sig")
	s.AssertNotCalled(t, "StepUpdate", mock.Anything)
}

// TestFinalizeKilledWorkflow_NoStepsAtAllIsStillKilled is the never-started
// half of #349: a workflow reconciled before any step row exists (or whose
// steps all vanished) must not fall into WorkflowStatus's all-success
// default over an empty slice.
func TestFinalizeKilledWorkflow_NoStepsAtAllIsStillKilled(t *testing.T) {
	wf := &model.Workflow{ID: 10, Children: []*model.Step{}}

	s := mocks.NewMockStore(t)
	s.On("WorkflowUpdate", mock.MatchedBy(func(w *model.Workflow) bool {
		return w.ID == 10 && w.State == model.StatusKilled && w.Error == "sig"
	})).Return(nil)

	FinalizeKilledWorkflow(context.Background(), s, wf, 1000, "sig")
}

// TestFinalizeKilledWorkflow_LoadsChildrenWhenNil covers the caller
// convenience both rpc.go and reconcile.go rely on: pass a workflow with no
// preloaded Children and the function fetches them itself.
func TestFinalizeKilledWorkflow_LoadsChildrenWhenNil(t *testing.T) {
	wf := &model.Workflow{ID: 10}
	step := &model.Step{ID: 1, State: model.StatusRunning}

	s := mocks.NewMockStore(t)
	s.On("StepListFromWorkflowFind", wf).Return([]*model.Step{step}, nil)
	s.On("StepUpdate", mock.Anything).Return(nil)
	s.On("WorkflowUpdate", mock.Anything).Return(nil)

	FinalizeKilledWorkflow(context.Background(), s, wf, 1000, "sig")
	assert.Equal(t, model.StatusKilled, step.State)
}

// TestFinalizeKilledWorkflow_LoadFailureLeavesChildrenNilButContinues — a
// failed load is logged, not fatal; the workflow still gets finalized (as
// "no steps at all", since Children stays nil/empty).
func TestFinalizeKilledWorkflow_LoadFailureLeavesChildrenNilButContinues(t *testing.T) {
	wf := &model.Workflow{ID: 10}

	s := mocks.NewMockStore(t)
	s.On("StepListFromWorkflowFind", wf).Return(nil, assert.AnError)
	s.On("WorkflowUpdate", mock.MatchedBy(func(w *model.Workflow) bool {
		return w.State == model.StatusKilled
	})).Return(nil)

	FinalizeKilledWorkflow(context.Background(), s, wf, 1000, "sig")
}

// TestFinalizeKilledWorkflow_StepUpdateFailureLoggedButContinues — a failed
// per-step persist doesn't abort the workflow-level finalization; the step
// still ends up in-memory Killed and the workflow update still happens.
func TestFinalizeKilledWorkflow_StepUpdateFailureLoggedButContinues(t *testing.T) {
	step := &model.Step{ID: 1, State: model.StatusRunning}
	wf := &model.Workflow{ID: 10, Children: []*model.Step{step}}

	s := mocks.NewMockStore(t)
	s.On("StepUpdate", mock.Anything).Return(assert.AnError)
	s.On("WorkflowUpdate", mock.MatchedBy(func(w *model.Workflow) bool {
		return w.State == model.StatusKilled
	})).Return(nil)

	FinalizeKilledWorkflow(context.Background(), s, wf, 1000, "sig")
	assert.Equal(t, model.StatusKilled, step.State)
}

// TestFinalizeKilledWorkflow_VerifyProbeReconcilesToSuccess exercises the
// #235 recovery path THROUGH FinalizeKilledWorkflow (not just
// ReconcileVerifiedKilledSteps in isolation, per verify_test.go): a killed
// step whose proof-query confirms the work landed flips to success, and the
// workflow — with its only step no longer Killed — is derived from the real
// outcome instead of being stamped Killed.
func TestFinalizeKilledWorkflow_VerifyProbeReconcilesToSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	orig := verifyHTTPClient
	verifyHTTPClient = srv.Client()
	t.Cleanup(func() { verifyHTTPClient = orig })

	step := &model.Step{ID: 1, State: model.StatusRunning, Verify: &model.StepVerify{URL: srv.URL}}
	wf := &model.Workflow{ID: 10, Children: []*model.Step{step}}

	s := mocks.NewMockStore(t)
	// First StepUpdate: the disconnect kill itself. Second: the #235 probe
	// flipping it back to success.
	s.On("StepUpdate", mock.MatchedBy(func(st *model.Step) bool {
		return st.State == model.StatusKilled
	})).Return(nil).Once()
	s.On("StepUpdate", mock.MatchedBy(func(st *model.Step) bool {
		return st.State == model.StatusSuccess
	})).Return(nil).Once()
	s.On("WorkflowUpdate", mock.MatchedBy(func(w *model.Workflow) bool {
		return w.State == model.StatusSuccess && w.Error == ""
	})).Return(nil)

	FinalizeKilledWorkflow(context.Background(), s, wf, 1000, "sig")
	assert.Equal(t, model.StatusSuccess, step.State, "verified step must reconcile to success")
}

// TestFinalizeKilledWorkflow_VerifyProbeMixedOutcomeStaysKilled — one step
// reconciles to success, a sibling has no Verify declared and so stays
// Killed. The workflow must still land on Killed: the recompute loop after
// ReconcileVerifiedKilledSteps has to find that surviving Killed step.
func TestFinalizeKilledWorkflow_VerifyProbeMixedOutcomeStaysKilled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	orig := verifyHTTPClient
	verifyHTTPClient = srv.Client()
	t.Cleanup(func() { verifyHTTPClient = orig })

	verified := &model.Step{ID: 1, State: model.StatusRunning, Verify: &model.StepVerify{URL: srv.URL}}
	unverified := &model.Step{ID: 2, State: model.StatusRunning} // no Verify — stays Killed
	wf := &model.Workflow{ID: 10, Children: []*model.Step{verified, unverified}}

	s := mocks.NewMockStore(t)
	s.On("StepUpdate", mock.MatchedBy(func(st *model.Step) bool {
		return st.State == model.StatusKilled
	})).Return(nil)
	s.On("StepUpdate", mock.MatchedBy(func(st *model.Step) bool {
		return st.State == model.StatusSuccess
	})).Return(nil).Once()
	s.On("WorkflowUpdate", mock.MatchedBy(func(w *model.Workflow) bool {
		return w.State == model.StatusKilled && w.Error == "sig"
	})).Return(nil)

	FinalizeKilledWorkflow(context.Background(), s, wf, 1000, "sig")
	assert.Equal(t, model.StatusSuccess, verified.State)
	assert.Equal(t, model.StatusKilled, unverified.State)
}

// TestFinalizeKilledWorkflow_DisconnectSignatureContract locks the #349
// design decision inline with the function it governs: passing the literal
// "agent disconnected" signature (the primary rpc.ReleaseAgentTasks path)
// must trip Workflow.KilledByAgentDisconnect(); passing anything else (the
// killOrphan backstop, which has no requeue follow-up) must not.
func TestFinalizeKilledWorkflow_DisconnectSignatureContract(t *testing.T) {
	step := &model.Step{ID: 1, State: model.StatusRunning}

	wfDisconnect := &model.Workflow{ID: 1, Children: []*model.Step{step}}
	s1 := mocks.NewMockStore(t)
	s1.On("StepUpdate", mock.Anything).Return(nil)
	s1.On("WorkflowUpdate", mock.Anything).Return(nil)
	FinalizeKilledWorkflow(context.Background(), s1, wfDisconnect, 1000, "agent disconnected")
	assert.True(t, wfDisconnect.KilledByAgentDisconnect())

	step2 := &model.Step{ID: 2, State: model.StatusRunning}
	wfReconcile := &model.Workflow{ID: 2, Children: []*model.Step{step2}}
	s2 := mocks.NewMockStore(t)
	s2.On("StepUpdate", mock.Anything).Return(nil)
	s2.On("WorkflowUpdate", mock.Anything).Return(nil)
	FinalizeKilledWorkflow(context.Background(), s2, wfReconcile, 1000, reconcileErrSignature)
	assert.False(t, wfReconcile.KilledByAgentDisconnect())
}
