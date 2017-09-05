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

package main_test

import (
	"bpm/config"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	yaml "gopkg.in/yaml.v2"

	"github.com/kr/pty"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
	"github.com/onsi/gomega/gexec"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	uuid "github.com/satori/go.uuid"
)

var _ = Describe("bpm", func() {
	var (
		command *exec.Cmd

		boshConfigPath,
		jobName,
		containerID,
		cfgPath,
		stdoutFileLocation,
		stderrFileLocation,
		runcRoot,
		bpmLogFileLocation string

		cfg *config.ProcessConfig
	)

	var writeConfig = func(jobName, procName string, cfg *config.ProcessConfig) string {
		cfgDir := filepath.Join(boshConfigPath, "jobs", jobName, "config", "bpm")
		err := os.MkdirAll(cfgDir, 0755)
		Expect(err).NotTo(HaveOccurred())

		path := filepath.Join(cfgDir, fmt.Sprintf("%s.yml", procName))
		Expect(os.RemoveAll(path)).To(Succeed())
		f, err := os.OpenFile(
			path,
			os.O_RDWR|os.O_CREATE,
			0644,
		)
		Expect(err).NotTo(HaveOccurred())

		data, err := yaml.Marshal(cfg)
		Expect(err).NotTo(HaveOccurred())

		n, err := f.Write(data)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(len(data)))

		return path
	}

	var runcCommand = func(args ...string) *exec.Cmd {
		args = append([]string{runcRoot}, args...)
		return exec.Command("runc", args...)
	}

	var newDefaultConfig = func(jobName string) *config.ProcessConfig {
		return &config.ProcessConfig{
			Executable: "/bin/bash",
			Args: []string{
				"-c",
				//This script traps the SIGTERM signal and kills the subsequent
				//commands referenced by the PID in the $child variable
				fmt.Sprintf(`trap "echo Signalled && kill -9 $child" SIGTERM;
					 echo Foo is $FOO &&
					  (>&2 echo "$FOO is Foo") &&
					  (echo "Dear Diary, Today I measured my beats per minute." > %s/sys/log/%s/foo.log) &&
					  sleep 5 &
					 child=$!;
					 wait $child`, boshConfigPath, jobName),
			},
			Env: []string{"FOO=BAR"},
		}
	}

	BeforeEach(func() {
		var err error

		boshConfigPath, err = ioutil.TempDir(bpmTmpDir, "bpm-main-test")
		Expect(err).NotTo(HaveOccurred())

		err = os.MkdirAll(filepath.Join(boshConfigPath, "packages"), 0755)
		Expect(err).NotTo(HaveOccurred())

		err = os.MkdirAll(filepath.Join(boshConfigPath, "data", "packages"), 0755)
		Expect(err).NotTo(HaveOccurred())

		err = os.MkdirAll(filepath.Join(boshConfigPath, "packages", "bpm", "bin"), 0755)
		Expect(err).NotTo(HaveOccurred())

		runcPath, err := exec.LookPath("runc")
		Expect(err).NotTo(HaveOccurred())

		err = os.Link(runcPath, filepath.Join(boshConfigPath, "packages", "bpm", "bin", "runc"))
		Expect(err).NotTo(HaveOccurred())

		jobName = fmt.Sprintf("bpm-test-%s", uuid.NewV4().String())
		containerID = jobName
		cfg = newDefaultConfig(jobName)

		stdoutFileLocation = filepath.Join(boshConfigPath, "sys", "log", jobName, fmt.Sprintf("%s.out.log", jobName))
		stderrFileLocation = filepath.Join(boshConfigPath, "sys", "log", jobName, fmt.Sprintf("%s.err.log", jobName))
		bpmLogFileLocation = filepath.Join(boshConfigPath, "sys", "log", jobName, "bpm.log")

		cfgPath = writeConfig(jobName, jobName, cfg)

		runcRoot = fmt.Sprintf("--root=%s", filepath.Join(boshConfigPath, "data", "bpm", "runc"))
	})

	AfterEach(func() {
		// using force, as we cannot delete a running container.
		err := runcCommand("delete", "--force", containerID).Run() // TODO: Assert on error when runc is updated to 1.0.0-rc4+
		if err != nil {
			fmt.Fprintf(GinkgoWriter, "WARNING: Failed to cleanup container: %s\n", err.Error())
		}

		if CurrentGinkgoTestDescription().Failed {
			fmt.Fprintf(GinkgoWriter, "STDOUT: %s\n", fileContents(stdoutFileLocation)())
			fmt.Fprintf(GinkgoWriter, "STDERR: %s\n", fileContents(stderrFileLocation)())
		}

		err = os.RemoveAll(boshConfigPath)
		Expect(err).NotTo(HaveOccurred())
	})

	runcState := func(cid string) specs.State {
		cmd := runcCommand("state", cid)
		cmd.Stderr = GinkgoWriter

		data, err := cmd.Output()
		Expect(err).NotTo(HaveOccurred())

		stateResponse := specs.State{}
		err = json.Unmarshal(data, &stateResponse)
		Expect(err).NotTo(HaveOccurred())

		return stateResponse
	}

	Context("start", func() {
		JustBeforeEach(func() {
			command = exec.Command(bpmPath, "start", jobName)
			command.Env = append(command.Env, fmt.Sprintf("BPM_BOSH_ROOT=%s", boshConfigPath))
		})

		It("runs the process in a container with a pidfile", func() {
			session, err := gexec.Start(command, GinkgoWriter, GinkgoWriter)
			Expect(err).NotTo(HaveOccurred())
			Eventually(session).Should(gexec.Exit(0))

			state := runcState(containerID)
			Expect(state.Status).To(Equal("running"))
			pidText, err := ioutil.ReadFile(filepath.Join(boshConfigPath, "sys", "run", "bpm", jobName, fmt.Sprintf("%s.pid", jobName)))
			Expect(err).NotTo(HaveOccurred())

			pid, err := strconv.Atoi(string(pidText))
			Expect(err).NotTo(HaveOccurred())
			Expect(pid).To(Equal(state.Pid))
		})

		It("redirects the processes stdout and stderr to a standard location", func() {
			Expect(stdoutFileLocation).NotTo(BeAnExistingFile())
			Expect(stderrFileLocation).NotTo(BeAnExistingFile())

			session, err := gexec.Start(command, GinkgoWriter, GinkgoWriter)
			Expect(err).NotTo(HaveOccurred())
			Eventually(session).Should(gexec.Exit(0))

			Eventually(fileContents(stdoutFileLocation)).Should(Equal("Foo is BAR\n"))
			Eventually(fileContents(stderrFileLocation)).Should(Equal("BAR is Foo\n"))
		})

		It("exposes the internal log directory for writing", func() {
			session, err := gexec.Start(command, GinkgoWriter, GinkgoWriter)
			Expect(err).NotTo(HaveOccurred())
			Eventually(session).Should(gexec.Exit(0))

			exampleLogLocation := filepath.Join(boshConfigPath, "sys", "log", jobName, "foo.log")
			Eventually(exampleLogLocation).Should(BeAnExistingFile())
			Eventually(fileContents(exampleLogLocation)).Should(Equal("Dear Diary, Today I measured my beats per minute.\n"))
		})

		It("logs bpm internal logs to a consistent location", func() {
			Expect(bpmLogFileLocation).NotTo(BeAnExistingFile())

			session, err := gexec.Start(command, GinkgoWriter, GinkgoWriter)
			Expect(err).NotTo(HaveOccurred())
			Eventually(session).Should(gexec.Exit(0))

			Eventually(fileContents(bpmLogFileLocation)).Should(ContainSubstring("bpm.start.starting"))
			Eventually(fileContents(bpmLogFileLocation)).Should(ContainSubstring("bpm.start.complete"))
		})

		Context("when the process config path is specified", func() {
			var (
				newCfgPath string
			)

			BeforeEach(func() {
				newCfgPath = filepath.Join(filepath.Dir(cfgPath), "new-cfg.yml")

				err := os.Rename(cfgPath, newCfgPath)
				Expect(err).NotTo(HaveOccurred())

				// To be extra safe
				Expect(cfgPath).NotTo(BeAnExistingFile())
			})

			It("uses the provided config path instead of the default", func() {
				command = exec.Command(bpmPath, "start", jobName, "-c", newCfgPath)
				command.Env = append(command.Env, fmt.Sprintf("BPM_BOSH_ROOT=%s", boshConfigPath))

				session, err := gexec.Start(command, GinkgoWriter, GinkgoWriter)
				Expect(err).NotTo(HaveOccurred())
				Eventually(session).Should(gexec.Exit(0))

				state := runcState(containerID)
				Expect(state.Status).To(Equal("running"))
				pidText, err := ioutil.ReadFile(filepath.Join(boshConfigPath, "sys", "run", "bpm", jobName, fmt.Sprintf("%s.pid", jobName)))
				Expect(err).NotTo(HaveOccurred())

				pid, err := strconv.Atoi(string(pidText))
				Expect(err).NotTo(HaveOccurred())
				Expect(pid).To(Equal(state.Pid))
			})
		})

		Context("when a pre_start hook is specified", func() {
			BeforeEach(func() {
				f, err := os.OpenFile(filepath.Join(boshConfigPath, "pre-start"), os.O_CREATE|os.O_RDWR, 0755)
				Expect(err).NotTo(HaveOccurred())

				_, err = f.Write([]byte(fmt.Sprintf(`#!/bin/bash
					echo "Pre Start executed" > %s
				`, stdoutFileLocation)))
				Expect(err).NotTo(HaveOccurred())
				Expect(f.Close()).To(Succeed())

				cfg.Hooks = &config.Hooks{
					PreStart: filepath.Join(boshConfigPath, "pre-start"),
				}

				writeConfig(jobName, jobName, cfg)
			})

			It("executs the pre-start prior to starting the process", func() {
				session, err := gexec.Start(command, GinkgoWriter, GinkgoWriter)
				Expect(err).NotTo(HaveOccurred())
				Eventually(session).Should(gexec.Exit(0))
				Eventually(fileContents(stdoutFileLocation)).Should(Equal("Pre Start executed\nFoo is BAR\n"))
			})
		})

		Context("when there is a persistent store", func() {
			var storeFile string

			BeforeEach(func() {
				Expect(os.MkdirAll(filepath.Join(boshConfigPath, "store"), 0700)).To(Succeed())

				storeFile = filepath.Join(boshConfigPath, "store", jobName, "data.txt")

				cfg.Args = []string{
					"-c",
					fmt.Sprintf(
						`echo "storing some data" > %s;
						sleep 5`,
						storeFile,
					),
				}

				writeConfig(jobName, jobName, cfg)
			})

			It("exposes `/var/vcap/store/<job-name>` as a writeable mount point", func() {
				session, err := gexec.Start(command, GinkgoWriter, GinkgoWriter)
				Expect(err).NotTo(HaveOccurred())
				Eventually(session).Should(gexec.Exit(0))
				Eventually(storeFile).Should(BeAnExistingFile())
				Eventually(fileContents(storeFile)).Should(Equal("storing some data\n"))
			})
		})

		Context("when the process flag is specified", func() {
			var procName string

			BeforeEach(func() {
				procName = "server"
				containerID = fmt.Sprintf("%s.%s", jobName, procName)

				stdoutFileLocation = filepath.Join(boshConfigPath, "sys", "log", jobName, fmt.Sprintf("%s.out.log", procName))
				stderrFileLocation = filepath.Join(boshConfigPath, "sys", "log", jobName, fmt.Sprintf("%s.err.log", procName))

				cfg := newDefaultConfig(jobName)
				cfg.Args = []string{
					"-c",
					`echo "alternate config out" && echo "alternate config err" 1>&2 && sleep 5`,
				}

				writeConfig(jobName, procName, cfg)
			})

			It("runs the process specified in the corresponding configuration file", func() {
				command = exec.Command(bpmPath, "start", jobName, "-p", procName)
				command.Env = append(command.Env, fmt.Sprintf("BPM_BOSH_ROOT=%s", boshConfigPath))

				session, err := gexec.Start(command, GinkgoWriter, GinkgoWriter)
				Expect(err).NotTo(HaveOccurred())
				Eventually(session).Should(gexec.Exit(0))

				state := runcState(containerID)
				Expect(state.Status).To(Equal("running"))
				pidText, err := ioutil.ReadFile(filepath.Join(boshConfigPath, "sys", "run", "bpm", jobName, fmt.Sprintf("%s.pid", procName)))
				Expect(err).NotTo(HaveOccurred())

				pid, err := strconv.Atoi(string(pidText))
				Expect(err).NotTo(HaveOccurred())
				Expect(pid).To(Equal(state.Pid))

				Eventually(fileContents(stdoutFileLocation)).Should(Equal("alternate config out\n"))
				Eventually(fileContents(stderrFileLocation)).Should(Equal("alternate config err\n"))
			})
		})

		Context("capabilities", func() {
			BeforeEach(func() {
				cfg.Executable = "/bin/bash"
				cfg.Args = []string{
					"-c",
					`cat /proc/1/status | grep CapEff`,
				}

				cfgPath = writeConfig(jobName, jobName, cfg)
			})

			It("has no effective capabilities", func() {
				session, err := gexec.Start(command, GinkgoWriter, GinkgoWriter)
				Expect(err).NotTo(HaveOccurred())
				Eventually(session).Should(gexec.Exit(0))
				Eventually(fileContents(stdoutFileLocation)).Should(MatchRegexp("^\\s?CapEff:\\s?0000000000000000\\s?$"))
			})
		})

		Context("resource limits", func() {
			Context("memory", func() {
				BeforeEach(func() {
					cfg.Executable = "/bin/bash"
					cfg.Args = []string{
						"-c",
						// See https://codegolf.stackexchange.com/questions/24485/create-a-memory-leak-without-any-fork-bombs
						`start_memory_leak() { :(){ : $@$@;};: : ;};
							trap "kill $child" SIGTERM;
							sleep 100 &
							child=$!;
							wait $child;
							start_memory_leak`,
					}

					limit := "4M"
					cfg.Limits = &config.Limits{
						Memory: &limit,
					}

					cfgPath = writeConfig(jobName, jobName, cfg)
				})

				streamOOMEvents := func(stdout io.Reader) chan event {
					oomEvents := make(chan event)

					decoder := json.NewDecoder(stdout)

					go func() {
						defer GinkgoRecover()
						defer close(oomEvents)

						for {
							var actualEvent event
							err := decoder.Decode(&actualEvent)
							if err != nil {
								return
							}

							if actualEvent.Type == "oom" {
								oomEvents <- actualEvent
							}
							time.Sleep(100 * time.Millisecond)
						}
					}()

					return oomEvents
				}

				It("gets OOMed when it exceeds its memory limit", func() {
					session, err := gexec.Start(command, GinkgoWriter, GinkgoWriter)
					Expect(err).NotTo(HaveOccurred())
					Eventually(session).Should(gexec.Exit(0))

					Eventually(func() string {
						return runcState(containerID).Status
					}).Should(Equal("running"))

					eventsCmd := runcCommand("events", containerID)
					stdout, err := eventsCmd.StdoutPipe()
					Expect(err).NotTo(HaveOccurred())

					oomEventsChan := streamOOMEvents(stdout)
					Expect(eventsCmd.Start()).To(Succeed())

					Expect(runcCommand("kill", containerID).Run()).To(Succeed())

					Eventually(oomEventsChan).Should(Receive())

					Expect(eventsCmd.Process.Kill()).To(Succeed())
					Eventually(oomEventsChan).Should(BeClosed())
				})
			})

			Context("open files", func() {
				BeforeEach(func() {
					cfg.Executable = "/bin/bash"
					cfg.Args = []string{
						"-c",
						fmt.Sprintf(`file_dir=%s;
						  start_file_leak() { for i in $(seq 1 20); do touch $file_dir/file-$i; done; tail -f $file_dir/* ;};
							trap "kill $child" SIGTERM;
							sleep 100 &
							child=$!;
							wait $child;
							start_file_leak`, filepath.Join(boshConfigPath, "data", jobName)),
					}

					limit := uint64(10)
					cfg.Limits = &config.Limits{
						OpenFiles: &limit,
					}

					cfgPath = writeConfig(jobName, jobName, cfg)
				})

				It("cannot open more files than permitted", func() {
					session, err := gexec.Start(command, GinkgoWriter, GinkgoWriter)
					Expect(err).NotTo(HaveOccurred())
					Eventually(session).Should(gexec.Exit(0))

					Eventually(func() string {
						return runcState(containerID).Status
					}).Should(Equal("running"))

					Expect(runcCommand("kill", containerID).Run()).To(Succeed())

					Eventually(fileContents(stderrFileLocation)).Should(ContainSubstring("Too many open files"))
				})
			})

			Context("processes", func() {
				BeforeEach(func() {
					cfg.Executable = "/bin/bash"
					cfg.Args = []string{
						"-c",
						` trap "kill $child" SIGTERM;
                    sleep 100 &
                    child=$!;
                    wait $child;
                    for i in $(seq 1 999); do sleep 100 & done;
                    wait`,
					}

					limit := int64(1000)
					cfg.Limits = &config.Limits{
						Processes: &limit,
					}

					cfgPath = writeConfig(jobName, jobName, cfg)
				})

				It("cannot create more processes than permitted", func() {
					session, err := gexec.Start(command, GinkgoWriter, GinkgoWriter)
					Expect(err).NotTo(HaveOccurred())
					Eventually(session).Should(gexec.Exit(0))

					Eventually(func() string {
						return runcState(containerID).Status
					}).Should(Equal("running"))

					Expect(runcCommand("kill", containerID).Run()).To(Succeed())

					Eventually(fileContents(stderrFileLocation)).Should(ContainSubstring("fork: retry: Resource temporarily unavailable"))
				})
			})

		})

		Context("namespaces", func() {
			Context("ipc", func() {
				var messageQueueId int

				BeforeEach(func() {
					ipcCmd := exec.Command("ipcmk", "-Q")
					output, err := ipcCmd.CombinedOutput()
					Expect(err).NotTo(HaveOccurred())

					parts := strings.Split(string(output), ":")
					Expect(parts).To(HaveLen(2))
					messageQueueId, err = strconv.Atoi(strings.Trim(parts[1], " \n"))
					Expect(err).NotTo(HaveOccurred())

					cfg.Args = []string{
						"-c",
						fmt.Sprintf(`
						ipcs -q -i %d;
						sleep 5
						`, messageQueueId),
					}

					writeConfig(jobName, jobName, cfg)
				})

				AfterEach(func() {
					ipcCmd := exec.Command("ipcrm", "-q", strconv.Itoa(messageQueueId))
					output, err := ipcCmd.CombinedOutput()
					Expect(err).NotTo(HaveOccurred(), string(output))
				})

				It("it can only see message queues in its own namespace", func() {
					session, err := gexec.Start(command, GinkgoWriter, GinkgoWriter)
					Expect(err).NotTo(HaveOccurred())
					Eventually(session).Should(gexec.Exit(0))

					Eventually(fileContents(stderrFileLocation)).Should(
						ContainSubstring(fmt.Sprintf("ipcs: id %d not found", messageQueueId)),
					)
				})
			})
		})

		Context("when the stdout and stderr files already exist", func() {
			BeforeEach(func() {
				Expect(os.MkdirAll(filepath.Dir(stdoutFileLocation), 0700)).To(Succeed())
				Expect(ioutil.WriteFile(stdoutFileLocation, []byte("STDOUT PREFIX: "), 0700)).To(Succeed())
				Expect(ioutil.WriteFile(stderrFileLocation, []byte("STDERR PREFIX: "), 0700)).To(Succeed())
			})

			It("does not truncate the file", func() {
				session, err := gexec.Start(command, GinkgoWriter, GinkgoWriter)
				Expect(err).NotTo(HaveOccurred())
				Eventually(session).Should(gexec.Exit(0))

				Eventually(fileContents(stdoutFileLocation)).Should(Equal("STDOUT PREFIX: Foo is BAR\n"))
				Eventually(fileContents(stderrFileLocation)).Should(Equal("STDERR PREFIX: BAR is Foo\n"))
			})
		})

		Context("when the bpm configuration file does not exist", func() {
			BeforeEach(func() {
				err := os.RemoveAll(cfgPath)
				Expect(err).NotTo(HaveOccurred())
			})

			It("exit with a non-zero exit code and prints an error", func() {
				command = exec.Command(bpmPath, "start", jobName)
				command.Env = append(command.Env, fmt.Sprintf("BPM_BOSH_ROOT=%s", boshConfigPath))

				session, err := gexec.Start(command, GinkgoWriter, GinkgoWriter)
				Expect(err).ShouldNot(HaveOccurred())
				Eventually(session).Should(gexec.Exit(1))
				Expect(session.Err).Should(gbytes.Say(fmt.Sprintf("%s.yml", jobName)))
			})
		})

		Context("when no job name is specified", func() {
			It("exits with a non-zero exit code and prints the usage", func() {
				command = exec.Command(bpmPath, "start")
				command.Env = append(command.Env, fmt.Sprintf("BPM_BOSH_ROOT=%s", boshConfigPath))

				session, err := gexec.Start(command, GinkgoWriter, GinkgoWriter)
				Expect(err).ShouldNot(HaveOccurred())
				Eventually(session).Should(gexec.Exit(1))

				Expect(session.Err).Should(gbytes.Say("must specify a job"))
			})
		})

		Context("when a running container exist with the same name", func() {
			startContainer := func() *exec.Cmd {
				cfg.Executable = "/bin/bash"
				cfg.Args = []string{
					"-c",
					"sleep 10000",
				}

				limit := int64(1000)
				cfg.Limits = &config.Limits{
					Processes: &limit,
				}

				cfgPath = writeConfig(jobName, jobName, cfg)

				start := exec.Command(bpmPath, "start", jobName)
				start.Env = append(start.Env, fmt.Sprintf("BPM_BOSH_ROOT=%s", boshConfigPath))
				return start
			}

			// Currently assumes only one thing is running #NAIVE
			getContainerPid := func() int {
				command = exec.Command(bpmPath, "list")
				command.Env = append(command.Env, fmt.Sprintf("BPM_BOSH_ROOT=%s", boshConfigPath))
				session, err := gexec.Start(command, GinkgoWriter, GinkgoWriter)
				Expect(err).ShouldNot(HaveOccurred())
				Eventually(session).Should(gexec.Exit(0))
				Expect(session.Out).Should(gbytes.Say("running"))

				re := regexp.MustCompile("\\s(\\d+)\\s")
				pids := re.FindSubmatch(session.Out.Contents())
				Expect(pids).ShouldNot(BeNil())
				Expect(len(pids)).Should(Equal(2))

				pid, err := strconv.Atoi(string(pids[1]))
				Expect(err).NotTo(HaveOccurred())
				return pid
			}

			BeforeEach(func() {
				start := startContainer()
				session, err := gexec.Start(start, GinkgoWriter, GinkgoWriter)
				Expect(err).ShouldNot(HaveOccurred())
				Eventually(session).Should(gexec.Exit(0))
			})

			It("should not restart the container and logs", func() {
				origPid := getContainerPid()
				start := startContainer()
				session, err := gexec.Start(start, GinkgoWriter, GinkgoWriter)
				Expect(err).ShouldNot(HaveOccurred())
				Eventually(session).Should(gexec.Exit(0))
				Expect(session.Out).Should(gbytes.Say(fmt.Sprintf("container %s is already running", jobName)))
				newPid := getContainerPid()
				Expect(newPid).To(Equal(origPid))
			})
		})

		Context("when a stopped container exists with the same name", func() {
			BeforeEach(func() {
				cfg.Executable = "/bin/bash"
				cfg.Args = []string{
					"-c",
					"sleep 10000",
				}

				limit := int64(1000)
				cfg.Limits = &config.Limits{
					Processes: &limit,
				}

				cfgPath = writeConfig(jobName, jobName, cfg)

				start := exec.Command(bpmPath, "start", jobName)
				start.Env = append(start.Env, fmt.Sprintf("BPM_BOSH_ROOT=%s", boshConfigPath))

				session, err := gexec.Start(start, GinkgoWriter, GinkgoWriter)
				Expect(err).ShouldNot(HaveOccurred())
				Eventually(session).Should(gexec.Exit(0))

				// kill the existing container
				command = exec.Command(bpmPath, "list")
				command.Env = append(command.Env, fmt.Sprintf("BPM_BOSH_ROOT=%s", boshConfigPath))
				session, err = gexec.Start(command, GinkgoWriter, GinkgoWriter)
				Expect(err).ShouldNot(HaveOccurred())
				Eventually(session).Should(gexec.Exit(0))
				Expect(session.Out).Should(gbytes.Say("running"))

				re := regexp.MustCompile("\\s(\\d+)\\s")
				pids := re.FindSubmatch(session.Out.Contents())
				Expect(pids).ShouldNot(BeNil())
				Expect(len(pids)).Should(Equal(2))

				pid, err := strconv.Atoi(string(pids[1]))
				Expect(err).NotTo(HaveOccurred())
				err = syscall.Kill(pid, syscall.Signal(9))
				Expect(err).NotTo(HaveOccurred())

				// This is insane, killing takes time
				time.Sleep(1000 * time.Millisecond)

				// check that bpm thinks the container is stopped
				command = exec.Command(bpmPath, "list")
				command.Env = append(command.Env, fmt.Sprintf("BPM_BOSH_ROOT=%s", boshConfigPath))
				session, err = gexec.Start(command, GinkgoWriter, GinkgoWriter)
				Expect(err).ShouldNot(HaveOccurred())
				Eventually(session).Should(gexec.Exit(0))
				Expect(session.Out).Should(gbytes.Say("stopped"))
			})

			It("`bpm start` cleans up the associated container and artifacts and starts it", func() {
				session, err := gexec.Start(command, GinkgoWriter, GinkgoWriter)
				Expect(err).ShouldNot(HaveOccurred())
				Eventually(session).Should(gexec.Exit(0))

				command = exec.Command(bpmPath, "list")
				command.Env = append(command.Env, fmt.Sprintf("BPM_BOSH_ROOT=%s", boshConfigPath))
				session, err = gexec.Start(command, GinkgoWriter, GinkgoWriter)
				Expect(err).ShouldNot(HaveOccurred())
				Eventually(session).Should(gexec.Exit(0))

				Expect(session.Out).Should(gbytes.Say("running"))
			})

			Context("when the cleanup fails", func() {
				AfterEach(func() {
					newPath := filepath.Join(boshConfigPath, "jobs", jobName, "config", "bpm", "foo.yml")
					oldPath := filepath.Join(boshConfigPath, "jobs", jobName, "config", "bpm", fmt.Sprintf("%s.yml", jobName))
					os.Rename(newPath, oldPath)

					// just to be doubly sure it's definitely moved!
					_, err := os.Open(newPath)
					Expect(err).To(HaveOccurred())
					Expect(os.IsNotExist(err)).To(BeTrue())
				})

				It("gives instructions on where to manually cleanup", func() {
					oldPath := filepath.Join(boshConfigPath, "jobs", jobName, "config", "bpm", fmt.Sprintf("%s.yml", jobName))
					newPath := filepath.Join(boshConfigPath, "jobs", jobName, "config", "bpm", "foo.yml")
					os.Rename(oldPath, newPath)

					// just to be doubly sure it's definitely moved!
					_, err := os.Open(oldPath)
					Expect(err).To(HaveOccurred())
					Expect(os.IsNotExist(err)).To(BeTrue())

					session, err := gexec.Start(command, GinkgoWriter, GinkgoWriter)
					Expect(err).ShouldNot(HaveOccurred())
					Eventually(session).Should(gexec.Exit(1))
					Expect(session.Err).Should(gbytes.Say("no such file or directory"))

					command = exec.Command(bpmPath, "list")
					command.Env = append(command.Env, fmt.Sprintf("BPM_BOSH_ROOT=%s", boshConfigPath))

					session, err = gexec.Start(command, GinkgoWriter, GinkgoWriter)
					Expect(err).ShouldNot(HaveOccurred())
					Eventually(session).Should(gexec.Exit(0))

					Expect(session.Out).Should(gbytes.Say("stopped"))
				})
			})
		})
	})

	Context("stop", func() {
		BeforeEach(func() {
			startCmd := exec.Command(bpmPath, "start", jobName)
			startCmd.Env = append(startCmd.Env, fmt.Sprintf("BPM_BOSH_ROOT=%s", boshConfigPath))

			session, err := gexec.Start(startCmd, GinkgoWriter, GinkgoWriter)
			Expect(err).ShouldNot(HaveOccurred())
			Eventually(session).Should(gexec.Exit(0))
		})

		JustBeforeEach(func() {
			command = exec.Command(bpmPath, "stop", jobName)
			command.Env = append(command.Env, fmt.Sprintf("BPM_BOSH_ROOT=%s", boshConfigPath))
		})

		It("signals the container with a SIGTERM", func() {
			session, err := gexec.Start(command, GinkgoWriter, GinkgoWriter)
			Expect(err).ToNot(HaveOccurred())
			Eventually(session).Should(gexec.Exit(0))

			Eventually(fileContents(stdoutFileLocation)).Should(ContainSubstring("Signalled"))
		})

		It("removes the container and its corresponding process", func() {
			session, err := gexec.Start(command, GinkgoWriter, GinkgoWriter)
			Expect(err).ToNot(HaveOccurred())
			Eventually(session).Should(gexec.Exit(0))

			Expect(runcCommand("state", containerID).Run()).To(HaveOccurred())
		})

		It("removes the bundle directory", func() {
			session, err := gexec.Start(command, GinkgoWriter, GinkgoWriter)
			Expect(err).NotTo(HaveOccurred())
			Eventually(session).Should(gexec.Exit(0))

			_, err = os.Open(filepath.Join(boshConfigPath, "data", "bpm", "bundles", jobName, jobName))
			Expect(err).To(HaveOccurred())
			Expect(os.IsNotExist(err)).To(BeTrue())
		})

		It("logs bpm internal logs to a consistent location", func() {
			session, err := gexec.Start(command, GinkgoWriter, GinkgoWriter)
			Expect(err).NotTo(HaveOccurred())
			Eventually(session).Should(gexec.Exit(0))

			Eventually(fileContents(bpmLogFileLocation)).Should(ContainSubstring("bpm.stop.starting"))
			Eventually(fileContents(bpmLogFileLocation)).Should(ContainSubstring("bpm.stop.complete"))
		})

		Context("when the job name is not specified", func() {
			It("exits with a non-zero exit code and prints the usage", func() {
				command = exec.Command(bpmPath, "stop")
				command.Env = append(command.Env, fmt.Sprintf("BPM_BOSH_ROOT=%s", boshConfigPath))

				session, err := gexec.Start(command, GinkgoWriter, GinkgoWriter)
				Expect(err).ShouldNot(HaveOccurred())
				Eventually(session).Should(gexec.Exit(1))

				Expect(session.Err).Should(gbytes.Say("must specify a job"))
			})
		})

		Context("when the job is already stopped", func() {
			It("returns successfully", func() {
				firstSession, err := gexec.Start(command, GinkgoWriter, GinkgoWriter)
				Expect(err).ShouldNot(HaveOccurred())
				Eventually(firstSession).Should(gexec.Exit(0))

				secondCommand := exec.Command(bpmPath, "stop", jobName)
				secondCommand.Env = append(secondCommand.Env, fmt.Sprintf("BPM_BOSH_ROOT=%s", boshConfigPath))

				secondSession, err := gexec.Start(secondCommand, GinkgoWriter, GinkgoWriter)
				Expect(err).ShouldNot(HaveOccurred())

				Eventually(secondSession).Should(gexec.Exit(0))
				Expect(secondSession.Out).Should(gbytes.Say("job-already-stopped"))
			})
		})

		Context("when an invalid job/process name is specified", func() {
			BeforeEach(func() {
				jobName = "some-bad-job-name"
			})

			It("returns successfully", func() {
				session, err := gexec.Start(command, GinkgoWriter, GinkgoWriter)
				Expect(err).ShouldNot(HaveOccurred())

				Eventually(session).Should(gexec.Exit(0))
				Expect(session.Out).Should(gbytes.Say("job-already-stopped"))
			})
		})
	})

	Context("list", func() {
		Context("with running and stopped containers", func() {
			var otherJobName, otherProcName string

			BeforeEach(func() {
				startCmd := exec.Command(bpmPath, "start", jobName)
				startCmd.Env = append(startCmd.Env, fmt.Sprintf("BPM_BOSH_ROOT=%s", boshConfigPath))

				session, err := gexec.Start(startCmd, GinkgoWriter, GinkgoWriter)
				Expect(err).ShouldNot(HaveOccurred())
				Eventually(session).Should(gexec.Exit(0))

				otherJobName = "example-2"
				otherProcName = "server"
				otherCfg := newDefaultConfig(otherJobName)
				writeConfig(otherJobName, otherProcName, otherCfg)

				startCmd = exec.Command(bpmPath, "start", otherJobName, "-p", otherProcName)
				startCmd.Env = append(startCmd.Env, fmt.Sprintf("BPM_BOSH_ROOT=%s", boshConfigPath))

				session, err = gexec.Start(startCmd, GinkgoWriter, GinkgoWriter)
				Expect(err).ShouldNot(HaveOccurred())
				Eventually(session).Should(gexec.Exit(0))
			})

			It("lists the running jobs and their state", func() {
				listCmd := exec.Command(bpmPath, "list")
				listCmd.Env = append(listCmd.Env, fmt.Sprintf("BPM_BOSH_ROOT=%s", boshConfigPath))

				session, err := gexec.Start(listCmd, GinkgoWriter, GinkgoWriter)
				Expect(err).ShouldNot(HaveOccurred())

				state := runcState(containerID)
				otherState := runcState(fmt.Sprintf("%s.%s", otherJobName, otherProcName))

				Eventually(session).Should(gexec.Exit(0))
				Expect(session.Out).Should(gbytes.Say("Name\\s+Pid\\s+Status"))
				Expect(session.Out).Should(gbytes.Say(fmt.Sprintf("%s\\s+%d\\s+%s", state.ID, state.Pid, state.Status)))
				Expect(session.Out).Should(gbytes.Say(fmt.Sprintf("%s\\s+%d\\s+%s", otherState.ID, otherState.Pid, otherState.Status)))
			})
		})

		Context("when no containers are running", func() {
			It("prints no output", func() {
				listCmd := exec.Command(bpmPath, "list")
				listCmd.Env = append(listCmd.Env, fmt.Sprintf("BPM_BOSH_ROOT=%s", boshConfigPath))

				session, err := gexec.Start(listCmd, GinkgoWriter, GinkgoWriter)
				Expect(err).ShouldNot(HaveOccurred())

				Eventually(session).Should(gexec.Exit(0))
				Expect(session.Out).Should(gbytes.Say(""))
			})
		})
	})

	Context("pid", func() {
		var pidCmd *exec.Cmd

		BeforeEach(func() {
			pidCmd = exec.Command(bpmPath, "pid", jobName)
			pidCmd.Env = append(pidCmd.Env, fmt.Sprintf("BPM_BOSH_ROOT=%s", boshConfigPath))

			startCmd := exec.Command(bpmPath, "start", jobName)
			startCmd.Env = append(startCmd.Env, fmt.Sprintf("BPM_BOSH_ROOT=%s", boshConfigPath))

			session, err := gexec.Start(startCmd, GinkgoWriter, GinkgoWriter)
			Expect(err).ShouldNot(HaveOccurred())
			Eventually(session).Should(gexec.Exit(0))
		})

		Context("when the container is running", func() {
			It("returns the external pid", func() {
				session, err := gexec.Start(pidCmd, GinkgoWriter, GinkgoWriter)
				Expect(err).ShouldNot(HaveOccurred())

				state := runcState(containerID)
				Eventually(session).Should(gexec.Exit(0))
				Expect(session.Out).Should(gbytes.Say(fmt.Sprintf("%d", state.Pid)))
			})
		})

		Context("when the container is stopped", func() {
			BeforeEach(func() {
				Expect(runcCommand("kill", containerID, "KILL").Run()).To(Succeed())
				Eventually(func() string {
					return runcState(containerID).Status
				}).Should(Equal("stopped"))
			})

			It("returns an error", func() {
				session, err := gexec.Start(pidCmd, GinkgoWriter, GinkgoWriter)
				Expect(err).ShouldNot(HaveOccurred())

				Eventually(session).Should(gexec.Exit(1))
				Expect(session.Err).Should(gbytes.Say("Error: no pid for job"))
			})
		})

		Context("when the container does not exist", func() {
			BeforeEach(func() {
				stopCmd := exec.Command(bpmPath, "stop", jobName)
				stopCmd.Env = append(stopCmd.Env, fmt.Sprintf("BPM_BOSH_ROOT=%s", boshConfigPath))

				session, err := gexec.Start(stopCmd, GinkgoWriter, GinkgoWriter)
				Expect(err).ShouldNot(HaveOccurred())
				Eventually(session).Should(gexec.Exit(0))
			})

			It("returns an error", func() {
				session, err := gexec.Start(pidCmd, GinkgoWriter, GinkgoWriter)
				Expect(err).ShouldNot(HaveOccurred())

				Eventually(session).Should(gexec.Exit(1))
				Expect(session.Err).Should(gbytes.Say("Error: job is not running"))
			})
		})

		Context("when an invalid job/process name is specified", func() {
			BeforeEach(func() {
				path := os.Getenv("PATH")

				pidCmd = exec.Command(bpmPath, "pid", "some-bad-job-name")
				pidCmd.Env = append(pidCmd.Env, fmt.Sprintf("BPM_BOSH_ROOT=%s", boshConfigPath))
				pidCmd.Env = append(pidCmd.Env, fmt.Sprintf("PATH=%s", path))
			})

			It("returns an error", func() {
				session, err := gexec.Start(pidCmd, GinkgoWriter, GinkgoWriter)
				Expect(err).ShouldNot(HaveOccurred())

				Eventually(session).Should(gexec.Exit(1))
				Expect(session.Err).Should(gbytes.Say("Error: job is not running"))
			})
		})

		Context("when no job name is specified", func() {
			It("exits with a non-zero exit code and prints the usage", func() {
				command = exec.Command(bpmPath, "pid")
				command.Env = append(command.Env, fmt.Sprintf("BPM_BOSH_ROOT=%s", boshConfigPath))

				session, err := gexec.Start(command, GinkgoWriter, GinkgoWriter)
				Expect(err).ShouldNot(HaveOccurred())
				Eventually(session).Should(gexec.Exit(1))

				Expect(session.Err).Should(gbytes.Say("must specify a job"))
			})
		})
	})

	Context("trace", func() {
		var traceCmd *exec.Cmd

		BeforeEach(func() {
			path := os.Getenv("PATH")

			traceCmd = exec.Command(bpmPath, "trace", jobName)
			traceCmd.Env = append(traceCmd.Env, fmt.Sprintf("BPM_BOSH_ROOT=%s", boshConfigPath))
			traceCmd.Env = append(traceCmd.Env, fmt.Sprintf("PATH=%s", path))

			startCmd := exec.Command(bpmPath, "start", jobName)
			startCmd.Env = append(startCmd.Env, fmt.Sprintf("BPM_BOSH_ROOT=%s", boshConfigPath))

			session, err := gexec.Start(startCmd, GinkgoWriter, GinkgoWriter)
			Expect(err).ShouldNot(HaveOccurred())
			Eventually(session).Should(gexec.Exit(0))
		})

		It("streams the output of a reasonable strace command", func() {
			session, err := gexec.Start(traceCmd, GinkgoWriter, GinkgoWriter)
			Expect(err).ShouldNot(HaveOccurred())
			Eventually(session.Err).Should(gbytes.Say("wait4"))
		})

		Context("when the container is stopped", func() {
			BeforeEach(func() {
				Expect(runcCommand("kill", containerID, "KILL").Run()).To(Succeed())
				Eventually(func() string {
					return runcState(containerID).Status
				}).Should(Equal("stopped"))
			})

			It("returns an error", func() {
				session, err := gexec.Start(traceCmd, GinkgoWriter, GinkgoWriter)
				Expect(err).ShouldNot(HaveOccurred())

				Eventually(session).Should(gexec.Exit(1))
				Expect(session.Err).Should(gbytes.Say("Error: no pid for job"))
			})
		})

		Context("when the container does not exist", func() {
			BeforeEach(func() {
				stopCmd := exec.Command(bpmPath, "stop", jobName)
				stopCmd.Env = append(stopCmd.Env, fmt.Sprintf("BPM_BOSH_ROOT=%s", boshConfigPath))

				session, err := gexec.Start(stopCmd, GinkgoWriter, GinkgoWriter)
				Expect(err).ShouldNot(HaveOccurred())
				Eventually(session).Should(gexec.Exit(0))
			})

			It("returns an error", func() {
				session, err := gexec.Start(traceCmd, GinkgoWriter, GinkgoWriter)
				Expect(err).ShouldNot(HaveOccurred())

				Eventually(session).Should(gexec.Exit(1))
				Expect(session.Err).Should(gbytes.Say("Error: job is not running"))
			})
		})

		Context("when an invalid job/process name is specified", func() {
			BeforeEach(func() {
				path := os.Getenv("PATH")

				traceCmd = exec.Command(bpmPath, "trace", "some-bad-job-name")
				traceCmd.Env = append(traceCmd.Env, fmt.Sprintf("BPM_BOSH_ROOT=%s", boshConfigPath))
				traceCmd.Env = append(traceCmd.Env, fmt.Sprintf("PATH=%s", path))
			})

			It("returns an error", func() {
				session, err := gexec.Start(traceCmd, GinkgoWriter, GinkgoWriter)
				Expect(err).ShouldNot(HaveOccurred())

				Eventually(session).Should(gexec.Exit(1))
				Expect(session.Err).Should(gbytes.Say("Error: job is not running"))
			})
		})

		Context("when no job name is specified", func() {
			It("exits with a non-zero exit code and prints the usage", func() {
				command = exec.Command(bpmPath, "trace")
				command.Env = append(command.Env, fmt.Sprintf("BPM_BOSH_ROOT=%s", boshConfigPath))

				session, err := gexec.Start(command, GinkgoWriter, GinkgoWriter)
				Expect(err).ShouldNot(HaveOccurred())
				Eventually(session).Should(gexec.Exit(1))

				Expect(session.Err).Should(gbytes.Say("must specify a job"))
			})
		})
	})

	Context("shell", func() {
		var (
			shellCmd   *exec.Cmd
			ptyF, ttyF *os.File
		)

		BeforeEach(func() {
			path := os.Getenv("PATH")

			// Read this for more info http://www.linusakesson.net/programming/tty
			var err error
			ptyF, ttyF, err = pty.Open()
			Expect(err).ShouldNot(HaveOccurred())

			shellCmd = exec.Command(bpmPath, "shell", jobName)
			shellCmd.Env = append(shellCmd.Env, fmt.Sprintf("BPM_BOSH_ROOT=%s", boshConfigPath))
			shellCmd.Env = append(shellCmd.Env, fmt.Sprintf("PATH=%s", path))
			shellCmd.Env = append(shellCmd.Env, "TERM=xterm-256color")

			shellCmd.Stdin = ttyF
			shellCmd.Stdout = ttyF
			shellCmd.Stderr = ttyF
			shellCmd.SysProcAttr = &syscall.SysProcAttr{
				Setctty: true,
				Setsid:  true,
			}

			startCmd := exec.Command(bpmPath, "start", jobName)
			startCmd.Env = append(startCmd.Env, fmt.Sprintf("BPM_BOSH_ROOT=%s", boshConfigPath))

			session, err := gexec.Start(startCmd, GinkgoWriter, GinkgoWriter)
			Expect(err).ShouldNot(HaveOccurred())
			Eventually(session).Should(gexec.Exit(0))
		})

		AfterEach(func() {
			Expect(ptyF.Close()).To(Succeed())
		})

		It("attaches to a shell running inside the container", func() {
			session, err := gexec.Start(shellCmd, GinkgoWriter, GinkgoWriter)
			Expect(err).ShouldNot(HaveOccurred())
			Expect(ttyF.Close()).NotTo(HaveOccurred())

			// Validate TERM variable is set
			_, err = ptyF.Write([]byte("/bin/echo $TERM\n"))
			Expect(err).ShouldNot(HaveOccurred())
			Eventually(session.Out).Should(gbytes.Say("xterm-256color"))

			_, err = ptyF.Write([]byte("exit\n"))
			Expect(err).ShouldNot(HaveOccurred())

			Eventually(session).Should(gexec.Exit(0))
		})

		It("does not print the usage on invalid commands", func() {
			session, err := gexec.Start(shellCmd, GinkgoWriter, GinkgoWriter)
			Expect(err).ShouldNot(HaveOccurred())
			Expect(ttyF.Close()).NotTo(HaveOccurred())

			_, err = ptyF.Write([]byte("this is not a valid command\n"))
			Expect(err).ShouldNot(HaveOccurred())

			_, err = ptyF.Write([]byte("exit\n"))
			Expect(err).ShouldNot(HaveOccurred())

			Consistently(session.Out).ShouldNot(gbytes.Say("Usage:"))
			Consistently(session.Err).ShouldNot(gbytes.Say("Usage:"))
		})

		Context("when the container does not exist", func() {
			BeforeEach(func() {

				stopCmd := exec.Command(bpmPath, "stop", jobName)
				stopCmd.Env = append(stopCmd.Env, fmt.Sprintf("BPM_BOSH_ROOT=%s", boshConfigPath))

				session, err := gexec.Start(stopCmd, GinkgoWriter, GinkgoWriter)
				Expect(err).ShouldNot(HaveOccurred())
				Eventually(session).Should(gexec.Exit(0))
			})

			It("returns an error", func() {
				session, err := gexec.Start(shellCmd, GinkgoWriter, GinkgoWriter)
				Expect(err).ShouldNot(HaveOccurred())

				Eventually(session).Should(gexec.Exit(1))
				Expect(session.Err).Should(gbytes.Say("does not exist"))
			})
		})

		Context("when no job name is specified", func() {
			It("exits with a non-zero exit code and prints the usage", func() {
				command = exec.Command(bpmPath, "shell")
				command.Env = append(command.Env, fmt.Sprintf("BPM_BOSH_ROOT=%s", boshConfigPath))

				session, err := gexec.Start(command, GinkgoWriter, GinkgoWriter)
				Expect(err).ShouldNot(HaveOccurred())
				Eventually(session).Should(gexec.Exit(1))

				Expect(session.Err).Should(gbytes.Say("must specify a job"))
			})
		})

		Context("when an invalid job/process name is specified", func() {
			It("returns an error", func() {
				command = exec.Command(bpmPath, "shell", "some-bad-job-name")
				command.Env = append(command.Env, fmt.Sprintf("BPM_BOSH_ROOT=%s", boshConfigPath))

				session, err := gexec.Start(command, GinkgoWriter, GinkgoWriter)
				Expect(err).ShouldNot(HaveOccurred())

				Eventually(session).Should(gexec.Exit(1))
				Expect(session.Err).Should(gbytes.Say("does not exist"))
			})
		})
	})

	Context("start stop parallelization", func() {
		BeforeEach(func() {
			cfg.Executable = "/bin/bash"
			cfg.Args = []string{
				"-c",
				`trap "kill $child" SIGUSR1;
				 sleep 100 &
				 child=$!;
				 wait $child;`,
			}

			cfgPath = writeConfig(jobName, jobName, cfg)
		})

		JustBeforeEach(func() {
			command = exec.Command(bpmPath, "start", jobName)
			command.Env = append(command.Env, fmt.Sprintf("BPM_BOSH_ROOT=%s", boshConfigPath))

			session, err := gexec.Start(command, GinkgoWriter, GinkgoWriter)
			Expect(err).ShouldNot(HaveOccurred())
			Eventually(session).Should(gexec.Exit(0))
		})

		It("serializes calls to start and stop", func() {
			stopCmd := exec.Command(bpmPath, "stop", jobName)
			stopCmd.Env = append(stopCmd.Env, fmt.Sprintf("BPM_BOSH_ROOT=%s", boshConfigPath))

			stopSesh, err := gexec.Start(stopCmd, GinkgoWriter, GinkgoWriter)
			Expect(err).ShouldNot(HaveOccurred())
			Consistently(stopSesh).ShouldNot(gexec.Exit())

			startCmd := exec.Command(bpmPath, "start", jobName)
			startCmd.Env = append(startCmd.Env, fmt.Sprintf("BPM_BOSH_ROOT=%s", boshConfigPath))

			startSesh, err := gexec.Start(startCmd, GinkgoWriter, GinkgoWriter)
			Expect(err).ShouldNot(HaveOccurred())
			Consistently(startSesh).ShouldNot(gexec.Exit())

			Expect(runcCommand("kill", containerID, "USR1").Run()).To(Succeed())

			Eventually(stopSesh).Should(gexec.Exit(0))
			Eventually(startSesh).Should(gexec.Exit(0))
		})
	})

	Context("when not run as root", func() {
		var tmpDir string

		BeforeEach(func() {
			var err error
			tmpDir, err = ioutil.TempDir("", "vcap-bpm")
			Expect(err).NotTo(HaveOccurred())

			f, err := os.Create(filepath.Join(tmpDir, "bpm"))
			Expect(err).NotTo(HaveOccurred())
			defer f.Close()

			bpmFile, err := os.Open(bpmPath)
			Expect(err).NotTo(HaveOccurred())
			defer bpmFile.Close()

			_, err = io.Copy(f, bpmFile)
			Expect(err).NotTo(HaveOccurred())

			err = os.Chmod(filepath.Join(tmpDir, "bpm"), 0777)
			Expect(err).NotTo(HaveOccurred())

			// 2000 and 3000 are test fixtures in the docker container
			err = chownR(tmpDir, 2000, 3000)
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			err := os.RemoveAll(tmpDir)
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns an error", func() {
			command := exec.Command(filepath.Join(tmpDir, "bpm"))
			command.SysProcAttr = &syscall.SysProcAttr{}
			command.SysProcAttr.Credential = &syscall.Credential{Uid: 2000, Gid: 3000}

			session, err := gexec.Start(command, GinkgoWriter, GinkgoWriter)
			Expect(err).NotTo(HaveOccurred())
			Eventually(session).Should(gexec.Exit(1))

			Expect(session.Err).ShouldNot(gbytes.Say("Usage:"))
			Expect(session.Err).Should(gbytes.Say("bpm must be run as root. Please run 'sudo -i' to become the root user."))
		})
	})

	Context("when no arguments are provided", func() {
		It("exits with a non-zero exit code and prints the usage", func() {
			command := exec.Command(bpmPath)
			session, err := gexec.Start(command, GinkgoWriter, GinkgoWriter)
			Expect(err).ShouldNot(HaveOccurred())
			Eventually(session).Should(gexec.Exit(1))

			Expect(session.Err).Should(gbytes.Say("Usage:"))
		})
	})
})

func chownR(path string, uid, gid int) error {
	return filepath.Walk(path, func(name string, _ os.FileInfo, err error) error {
		if err == nil {
			err = os.Chown(name, uid, gid)
		}
		return err
	})
}

func fileContents(path string) func() string {
	return func() string {
		data, err := ioutil.ReadFile(path)
		Expect(err).NotTo(HaveOccurred())
		return string(data)
	}
}

type event struct {
	Data containerStats `json:"data"`
	Type string         `json:"type"`
	ID   string         `json:"id"`
}

type containerStats struct {
	Memory memory `json:"memory"`
}

type memory struct {
	Cache     uint64            `json:"cache,omitempty"`
	Usage     memoryEntry       `json:"usage,omitempty"`
	Swap      memoryEntry       `json:"swap,omitempty"`
	Kernel    memoryEntry       `json:"kernel,omitempty"`
	KernelTCP memoryEntry       `json:"kernelTCP,omitempty"`
	Raw       map[string]uint64 `json:"raw,omitempty"`
}

type memoryEntry struct {
	Limit   uint64 `json:"limit"`
	Usage   uint64 `json:"usage,omitempty"`
	Max     uint64 `json:"max,omitempty"`
	Failcnt uint64 `json:"failcnt"`
}
