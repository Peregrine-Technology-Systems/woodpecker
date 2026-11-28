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

package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sync"

	"github.com/rs/zerolog/log"
	"github.com/urfave/cli/v3"

	"go.woodpecker-ci.org/woodpecker/v3/pipeline/backend/types"
)

type workflowState struct {
	stepState       sync.Map // map of *stepState
	baseDir         string
	homeDir         string
	workspaceDir    string
	pluginGitBinary string
	cgroup          *workflowCgroup // nil when the workflow is not contained (#412)
	cgroupRoot      string
}

type stepState struct {
	cmd    *exec.Cmd
	output io.ReadCloser
	cgroup *workflowCgroup // the step's own cgroup when contained (#423)
}

type local struct {
	tempDir         string
	isolatedHome    bool
	workflows       sync.Map
	pluginGitBinary string
	os, arch        string
	contain         bool // run each workflow in its own cgroup (#412)
	cgroups         *cgroupFS
}

var CLIWorkaroundExecAtDir string // To handle edge case for running local backend via cli exec

// New returns a new local Backend.
func New() types.Backend {
	return &local{
		os:   runtime.GOOS,
		arch: runtime.GOARCH,
	}
}

// NewAgent returns the local backend as the agent runs it. On Linux every
// workflow is contained in its own cgroup and killed as a whole when it ends,
// and a workflow that cannot be contained is refused (#412). Elsewhere it is
// New. woodpecker-cli exec keeps using New.
func NewAgent() types.Backend {
	return &local{
		os:      runtime.GOOS,
		arch:    runtime.GOARCH,
		contain: containmentSupported,
		cgroups: newCgroupFS(),
	}
}

func (e *local) Name() string {
	return "local"
}

func (e *local) IsAvailable(ctx context.Context) bool {
	if c, ok := ctx.Value(types.CliCommand).(*cli.Command); ok {
		if c.String("backend-engine") == e.Name() {
			return true
		}
	}
	_, inContainer := os.LookupEnv("WOODPECKER_IN_CONTAINER")
	return !inContainer
}

func (e *local) Flags() []cli.Flag {
	return Flags
}

func (e *local) Load(ctx context.Context) (*types.BackendInfo, error) {
	c, ok := ctx.Value(types.CliCommand).(*cli.Command)
	if ok {
		e.tempDir = c.String("backend-local-temp-dir")
		e.isolatedHome = c.Bool("backend-local-isolated-home")
	}

	e.loadClone()

	if e.contain {
		// Clear what a previous agent left. A resolve failure is not fatal
		// here: every workflow is then refused in SetupWorkflow, by name.
		root, own, err := e.cgroups.resolve()
		if err != nil {
			log.Error().Err(err).Msg("local backend: workflows will be refused until this is fixed")
		} else {
			log.Info().Str("root", root).Msg("local backend: each workflow runs in its own cgroup")
			e.cgroups.sweep(root, own)
		}
	} else if containmentSupported {
		log.Warn().Msg("local backend: workflows run uncontained (woodpecker-cli exec), so a child a step detaches can outlive it")
	}

	return &types.BackendInfo{
		Platform: e.os + "/" + e.arch,
	}, nil
}

func (e *local) SetupWorkflow(_ context.Context, _ *types.Config, taskUUID string) error {
	log.Trace().Str("taskUUID", taskUUID).Msg("create workflow environment")

	// Contain first: a workflow that cannot be contained must not start,
	// and must leave nothing behind (#412).
	var cgroup *workflowCgroup
	var cgroupRoot string
	if e.contain {
		root, _, err := e.cgroups.resolve()
		if err != nil {
			return err
		}
		if cgroup, err = e.cgroups.create(root, taskUUID); err != nil {
			return err
		}
		cgroupRoot = root
	}

	baseDir, err := os.MkdirTemp(e.tempDir, "woodpecker-local-*")
	if err != nil {
		if cgroup != nil {
			err = errors.Join(err, e.cgroups.destroy(cgroupRoot, cgroup))
		}
		return err
	}

	state := &workflowState{
		baseDir:    baseDir,
		homeDir:    filepath.Join(baseDir, "home"),
		cgroup:     cgroup,
		cgroupRoot: cgroupRoot,
	}
	e.workflows.Store(taskUUID, state)

	if err := os.Mkdir(state.homeDir, 0o700); err != nil {
		return err
	}

	// normal workspace setup case
	if CLIWorkaroundExecAtDir == "" {
		state.workspaceDir = filepath.Join(baseDir, "workspace")
		if err := os.Mkdir(state.workspaceDir, 0o700); err != nil {
			return err
		}
	} else
	// setup workspace via internal flag signaled from cli exec to a specific dir
	{
		state.workspaceDir = CLIWorkaroundExecAtDir
		if stat, err := os.Stat(CLIWorkaroundExecAtDir); os.IsNotExist(err) {
			log.Debug().Msgf("create workspace directory '%s' set by internal flag", CLIWorkaroundExecAtDir)
			if err := os.Mkdir(state.workspaceDir, 0o700); err != nil {
				return err
			}
		} else if !stat.IsDir() {
			//nolint:forbidigo
			log.Fatal().Msg("This should never happen! internalExecDir was set to an non directory path!")
		}
	}

	e.workflows.Store(taskUUID, state)

	return nil
}

func (e *local) StartStep(ctx context.Context, step *types.Step, taskUUID string) error {
	log.Trace().Str("taskUUID", taskUUID).Msgf("start step %s", step.Name)

	state, err := e.getWorkflowState(taskUUID)
	if err != nil {
		return err
	}

	// Get environment variables
	env := os.Environ()
	for a, b := range step.Environment {
		// append allowed env vars to command env
		if !slices.Contains(notAllowedEnvVarOverwrites, a) {
			env = append(env, a+"="+b)
		}
	}

	if e.isolatedHome {
		env = append(env, "HOME="+state.homeDir)
		env = append(env, "USERPROFILE="+state.homeDir)
	}

	env = append(env, "CI_WORKSPACE="+state.workspaceDir)

	switch step.Type {
	case types.StepTypeClone:
		return e.execClone(ctx, step, state, env)
	case types.StepTypeCommands:
		return e.execCommands(ctx, step, state, env)
	case types.StepTypePlugin:
		return e.execPlugin(ctx, step, state, env)
	default:
		return ErrUnsupportedStepType
	}
}

func (e *local) WaitStep(ctx context.Context, step *types.Step, taskUUID string) (*types.State, error) {
	log.Trace().Str("taskUUID", taskUUID).Msgf("wait for step %s", step.Name)

	stepState := &types.State{
		Exited: true,
	}

	if err := ctx.Err(); err != nil {
		stepState.Error = err
		return stepState, nil
	}

	state, err := e.getStepState(taskUUID, step.UUID)
	if err != nil {
		return nil, err
	}

	if state.cmd == nil {
		return nil, errors.New("exec: step command not set up")
	}

	// normally we use cmd.Wait() to wait for *exec.Cmd, but cmd.StdoutPipe() tells us not
	// as Wait() would close the io pipe even if not all logs where read and send back
	// so we have to do use the underlying functions
	if state.cmd.Process == nil {
		return nil, errors.New("exec: not started")
	}
	if state.cmd.ProcessState == nil {
		cmdState, err := state.cmd.Process.Wait()
		if err != nil {
			return nil, err
		}
		if cmdState == nil {
			return nil, errors.New("exec: cmd state after Wait() can not be nil but is")
		}
		stepState.ExitCode = cmdState.ExitCode()
		// can be nil if step got canceled
		if state.cmd != nil {
			state.cmd.ProcessState = cmdState
		}
	} else {
		stepState.ExitCode = state.cmd.ProcessState.ExitCode()
	}

	return stepState, err
}

func (e *local) TailStep(_ context.Context, step *types.Step, taskUUID string) (io.ReadCloser, error) {
	state, err := e.getStepState(taskUUID, step.UUID)
	if err != nil {
		return nil, err
	} else if state.output == nil {
		return nil, ErrStepReaderNotFound
	}
	return state.output, nil
}

func (e *local) DestroyStep(_ context.Context, step *types.Step, taskUUID string) error {
	state, err := e.getStepState(taskUUID, step.UUID)
	if err != nil {
		if errors.Is(err, ErrStepStateNotFound) {
			return nil
		}
		return err
	}

	// As WaitStep can not use cmd.Wait() witch ensures the process already finished and
	// the io pipe is closed on process end, we make sure it is done.
	if state.output != nil {
		_ = state.output.Close()
		state.output = nil
	}
	if state.cmd != nil {
		_ = state.cmd.Cancel()
		state.cmd = nil
	}
	workflowState, err := e.getWorkflowState(taskUUID)
	if err != nil {
		return err
	}
	workflowState.stepState.Delete(step.UUID)

	// Kill what the step started, detached children included (#423). A step
	// cgroup that would not empty is an error and stays as evidence.
	if state.cgroup != nil && workflowState.cgroup != nil {
		sc := state.cgroup
		state.cgroup = nil
		if err := e.cgroups.destroyStep(workflowState.cgroup, sc); err != nil {
			log.Error().Err(err).Str("taskUUID", taskUUID).Str("step", step.Name).Msg("local backend: step processes survived teardown")
			return err
		}
	}
	return nil
}

func (e *local) DestroyWorkflow(_ context.Context, _ *types.Config, taskUUID string) error {
	log.Trace().Str("taskUUID", taskUUID).Msg("delete workflow environment")

	state, err := e.getWorkflowState(taskUUID)
	if err != nil {
		return err
	}

	// clean up steps not cleaned up because of context cancel or detached function
	state.stepState.Range(func(_, value any) bool {
		if state, ok := value.(*stepState); ok && state != nil {
			if state.output != nil {
				_ = state.output.Close()
				state.output = nil
			}
			if state.cmd != nil {
				_ = state.cmd.Cancel()
				state.cmd = nil
			}
			if state.cgroup != nil {
				state.cgroup.close() // the workflow kill below covers it (#423)
			}
		}
		return true
	})

	// Kill everything the workflow started, detached children included,
	// and verify it is gone (#412). A cgroup that would not empty is an
	// error and stays in place as evidence.
	var cgroupErr error
	if state.cgroup != nil {
		if cgroupErr = e.cgroups.destroy(state.cgroupRoot, state.cgroup); cgroupErr != nil {
			log.Error().Err(cgroupErr).Str("taskUUID", taskUUID).Msg("local backend: workflow processes survived teardown")
		}
		state.cgroup = nil
	}

	if err := os.RemoveAll(state.baseDir); err != nil {
		return errors.Join(cgroupErr, err)
	}

	// hint for the gc to clean stuff
	state.stepState.Clear()
	e.workflows.Delete(taskUUID)

	return cgroupErr
}

func (e *local) getWorkflowState(taskUUID string) (*workflowState, error) {
	state, ok := e.workflows.Load(taskUUID)
	if !ok {
		return nil, ErrWorkflowStateNotFound
	}

	s, ok := state.(*workflowState)
	if !ok || s == nil {
		return nil, fmt.Errorf("could not parse state: %v", state)
	}

	return s, nil
}

func (e *local) getStepState(taskUUID, stepUUID string) (*stepState, error) {
	wState, err := e.getWorkflowState(taskUUID)
	if err != nil {
		return nil, err
	}

	state, ok := wState.stepState.Load(stepUUID)
	if !ok {
		return nil, ErrStepStateNotFound
	}

	s, ok := state.(*stepState)
	if !ok || s == nil {
		return nil, fmt.Errorf("could not parse state: %v", state)
	}

	return s, nil
}
