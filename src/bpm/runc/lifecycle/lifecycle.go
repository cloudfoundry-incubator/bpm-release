// Copyright (C) 2017-Present Pivotal Software, Inc. All rights reserved.
//
// This program and the accompanying materials are made available under
// the terms of the under the Apache License, Version 2.0 (the "License”);
// you may not use this file except in compliance with the License.
//
// You may obtain a copy of the License at
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.  See the
// License for the specific language governing permissions and limitations
// under the License.

package lifecycle

import (
	"bpm/config"
	"bpm/models"
	"bpm/runc/client"
	"bpm/usertools"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"code.cloudfoundry.org/clock"
	"code.cloudfoundry.org/lager"
)

const (
	ContainerSigQuitGracePeriod = 5 * time.Second
	ContainerStatePollInterval  = 1 * time.Second
	ContainerStateRunning       = "running"
	ContainerStatePaused        = "paused"
	ContainerStateStopped       = "stopped"
)

var TimeoutError = errors.New("failed to stop job within timeout")

//go:generate counterfeiter . UserFinder

type UserFinder interface {
	Lookup(username string) (specs.User, error)
}

//go:generate counterfeiter . CommandRunner

type CommandRunner interface {
	Run(*exec.Cmd) error
}

//go:generate counterfeiter . RuncAdapter

type RuncAdapter interface {
	CreateJobPrerequisites(bpmCfg *config.BPMConfig, procCfg *config.ProcessConfig, user specs.User) (*os.File, *os.File, error)
	BuildSpec(bpmCfg *config.BPMConfig, procCfg *config.ProcessConfig, user specs.User) (specs.Spec, error)
}

//go:generate counterfeiter . RuncClient

type RuncClient interface {
	CreateBundle(bundlePath string, jobSpec specs.Spec, user specs.User) error
	RunContainer(pidFilePath, bundlePath, containerID string, stdout, stderr io.Writer) error
	Exec(containerID, command string, stdin io.Reader, stdout, stderr io.Writer) error
	ContainerState(containerID string) (*specs.State, error)
	ListContainers() ([]client.ContainerState, error)
	SignalContainer(containerID string, signal client.Signal) error
	DeleteContainer(containerID string) error
	DestroyBundle(bundlePath string) error
}

type RuncLifecycle struct {
	clock         clock.Clock
	commandRunner CommandRunner
	runcAdapter   RuncAdapter
	runcClient    RuncClient
	userFinder    UserFinder
}

func NewRuncLifecycle(
	runcClient RuncClient,
	runcAdapter RuncAdapter,
	userFinder UserFinder,
	commandRunner CommandRunner,
	clock clock.Clock,
) *RuncLifecycle {
	return &RuncLifecycle{
		clock:         clock,
		runcClient:    runcClient,
		runcAdapter:   runcAdapter,
		userFinder:    userFinder,
		commandRunner: commandRunner,
	}
}

func (j *RuncLifecycle) StartJob(bpmCfg *config.BPMConfig, procCfg *config.ProcessConfig) error {
	user, err := j.userFinder.Lookup(usertools.VcapUser)
	if err != nil {
		return err
	}

	stdout, stderr, err := j.runcAdapter.CreateJobPrerequisites(bpmCfg, procCfg, user)
	if err != nil {
		return fmt.Errorf("failed to create system files: %s", err.Error())
	}
	defer stdout.Close()
	defer stderr.Close()

	spec, err := j.runcAdapter.BuildSpec(bpmCfg, procCfg, user)
	if err != nil {
		return err
	}

	err = j.runcClient.CreateBundle(bpmCfg.BundlePath(), spec, user)
	if err != nil {
		return fmt.Errorf("bundle build failure: %s", err.Error())
	}

	return j.runcClient.RunContainer(
		bpmCfg.PidFile(),
		bpmCfg.BundlePath(),
		bpmCfg.ContainerID(),
		stdout,
		stderr,
	)
}

// GetJob returns the following:
// - job, nil if the job is running (and no errors were encountered)
// - nil,nil if the job is not running and there is no other error
// - nil,error if there is any other error getting the job beyond it not running
func (j *RuncLifecycle) GetJob(cfg *config.BPMConfig) (*models.Job, error) {
	container, err := j.runcClient.ContainerState(cfg.ContainerID())
	if err != nil {
		return nil, err
	}

	if container == nil {
		return nil, nil
	}

	return &models.Job{
		Name:   container.ID,
		Pid:    container.Pid,
		Status: container.Status,
	}, nil
}

func (j *RuncLifecycle) OpenShell(cfg *config.BPMConfig, stdin io.Reader, stdout, stderr io.Writer) error {
	return j.runcClient.Exec(cfg.ContainerID(), "/bin/bash", stdin, stdout, stderr)
}

func (j *RuncLifecycle) ListJobs() ([]models.Job, error) {
	containers, err := j.runcClient.ListContainers()
	if err != nil {
		return nil, err
	}

	var jobs []models.Job
	for _, c := range containers {
		job := models.Job{
			Name:   c.ID,
			Pid:    c.InitProcessPid,
			Status: c.Status,
		}
		jobs = append(jobs, job)
	}

	return jobs, nil
}

func (j *RuncLifecycle) StopJob(logger lager.Logger, cfg *config.BPMConfig, exitTimeout time.Duration) error {
	err := j.runcClient.SignalContainer(cfg.ContainerID(), client.Term)
	if err != nil {
		return err
	}

	state, err := j.runcClient.ContainerState(cfg.ContainerID())
	if err != nil {
		logger.Error("failed-to-fetch-state", err)
	} else {
		if state.Status == "stopped" {
			return nil
		}
	}

	timeout := j.clock.NewTimer(exitTimeout)
	stateTicker := j.clock.NewTicker(ContainerStatePollInterval)
	defer stateTicker.Stop()

	for {
		select {
		case <-stateTicker.C():
			state, err = j.runcClient.ContainerState(cfg.ContainerID())
			if err != nil {
				logger.Error("failed-to-fetch-state", err)
			} else {
				if state.Status == "stopped" {
					return nil
				}
			}
		case <-timeout.C():
			err := j.runcClient.SignalContainer(cfg.ContainerID(), client.Quit)
			if err != nil {
				logger.Error("failed-to-sigquit", err)
			}

			j.clock.Sleep(ContainerSigQuitGracePeriod)
			return TimeoutError
		}
	}
}

func (j *RuncLifecycle) RemoveJob(cfg *config.BPMConfig) error {
	err := j.runcClient.DeleteContainer(cfg.ContainerID())
	if err != nil {
		return err
	}

	return j.runcClient.DestroyBundle(cfg.BundlePath())
}

type commandRunner struct{}

func NewCommandRunner() CommandRunner          { return &commandRunner{} }
func (*commandRunner) Run(cmd *exec.Cmd) error { return cmd.Run() }
