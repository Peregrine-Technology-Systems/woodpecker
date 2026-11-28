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

package grpc

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/rs/zerolog/log"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/pipeline"
	"go.woodpecker-ci.org/woodpecker/v3/server/queue"
)

// ErrAgentNotWorkflowOwner refuses an agent acting on a workflow it does not
// hold (#418, the intent of upstream #6308/#6759).
var ErrAgentNotWorkflowOwner = errors.New("agent does not hold this workflow")

// checkWorkflowOwner allows the caller only on a workflow it holds. The queue
// is the authority for a running workflow: it sets the holder at claim and
// restore and clears it at requeue, so a workflow held by another agent is
// refused. A workflow no longer in the queue (a late log batch, a re-sent
// Done) falls back to the agent recorded on it at Init. mustHold is for Init,
// which only the agent that polled the workflow may call.
func (s *RPC) checkWorkflowOwner(agent *model.Agent, workflow *model.Workflow, mustHold bool) error {
	holder, err := s.queue.Holder(fmt.Sprint(workflow.ID))
	switch {
	case err == nil && holder == agent.ID:
		return nil
	case err == nil:
		return s.refuseNotOwner(agent, workflow, fmt.Sprintf("held by agent %d", holder))
	case !errors.Is(err, queue.ErrNotFound):
		return fmt.Errorf("cannot tell who holds workflow %d: %w", workflow.ID, err)
	case !mustHold && workflow.AgentID == agent.ID:
		return nil
	}
	return s.refuseNotOwner(agent, workflow, fmt.Sprintf("not running, recorded on agent %d", workflow.AgentID))
}

// checkWorkflowOwnerByID loads the workflow and applies checkWorkflowOwner,
// for the calls that carry only its ID (Extend, Wait).
func (s *RPC) checkWorkflowOwnerByID(agent *model.Agent, strWorkflowID string) error {
	workflowID, err := strconv.ParseInt(strWorkflowID, 10, 64)
	if err != nil {
		return err
	}
	workflow, err := s.store.WorkflowLoad(workflowID)
	if err != nil {
		return fmt.Errorf("cannot find workflow %d: %w", workflowID, err)
	}
	return s.checkWorkflowOwner(agent, workflow, false)
}

func (s *RPC) refuseNotOwner(agent *model.Agent, workflow *model.Workflow, why string) error {
	err := fmt.Errorf("%w: workflow %d is %s, not agent %d", ErrAgentNotWorkflowOwner, workflow.ID, why, agent.ID)
	log.Warn().Err(err).Int64("agent_id", agent.ID).Int64("workflow_id", workflow.ID).Msg("rpc: refusing an agent that does not hold the workflow")
	return err
}

// abandonedRejection makes a refused step update read, to the agent, as a
// step the server has killed: WS agents from pts.634 and gRPC agents from
// pts.642 stop the workflow on it (#394, #397) instead of retrying.
func abandonedRejection(err error) error {
	return fmt.Errorf("%w: %w", err, &pipeline.StepTerminalError{State: model.StatusKilled})
}

// checkStepInWorkflow refuses a step update for a step of another workflow,
// even one in the same pipeline (#418, from upstream #7547).
func checkStepInWorkflow(step *model.Step, workflow *model.Workflow) error {
	if step.PipelineID == workflow.PipelineID && step.PPID == workflow.PID {
		return nil
	}
	return fmt.Errorf("agent returned status with step uuid '%s' which does not belong to workflow %d", step.UUID, workflow.ID)
}
