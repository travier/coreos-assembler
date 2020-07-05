// Copyright 2015 CoreOS, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"strings"
	"time"

	systemddbus "github.com/coreos/go-systemd/v22/dbus"
	systemdjournal "github.com/coreos/go-systemd/v22/journal"
	"github.com/coreos/pkg/capnslog"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"

	"github.com/coreos/mantle/cli"
	"github.com/coreos/mantle/kola"
	"github.com/coreos/mantle/kola/register"

	// Register any tests that we may wish to execute in kolet.
	_ "github.com/coreos/mantle/kola/registry"
)

const (
	// From /usr/include/bits/siginfo-consts.h
	CLD_EXITED int32 = 1
	CLD_KILLED int32 = 2
)

// Reboot handling
// ---
//
// Rebooting is complicated!  The high level API we expose is the one defined by
// the Debian autopkgtest specification:
// https://salsa.debian.org/ci-team/autopkgtest/raw/master/doc/README.package-tests.rst
//
// Today kola has support for rebooting a machine that ends up in a loop with SSH,
// checking the value of /proc/sys/kernel/random/boot_id.
// Originally we implemented the plain API that immediately starts a reboot by calling
// back to the harness API.  Now we want to support the `autopkgtest-reboot-prepare` API, so
// things are a bit more complicated, because the actual reboot is initiated by the client.
//
// The "immediate" reboot API is implemented in terms of the prepare API now.
//
// There are a few distinct actors here; using the term "subject" for the system
// under test:
//
// harness: The process running the coreos-assembler container
// login: The SSH login session initated on the subject (target) system
// unit: The systemd unit on the subject system running the test, currently named kola-runext.service
//
// We need to *synchronously* communicate state from the unit to back to the harness.  The
// login and unit also need to communicate to make this happen, because the channel
// between the harness and subject is SSH.
//
// The way this works today is that our implementation of the "reboot-prepare" binary
// writes the mark out to a file in /run and then starts a "sleep infinity" as a separate
// systemd unit and blocks on its termination.
//
// The login notices this unit was started, reads the mark file, then prints out the reboot
// data on stdout, which the harness reads.
//
// The harness then creates a separate SSH session which stops sshd (to avoid any races
// around logging in again), and then stops the "sleep infinity" service.
//
// At this point, the "mark" (or state saved between reboots) is safely on the harness,
// so the test code can invoke e.g. `reboot` or `reboot -ff` etc.
//
// The harness keeps polling via ssh, waiting until it can log in and also detects
// that the boot ID is different, and passes in the mark via an environment variable.

const (
	autopkgTestRebootPath   = "/tmp/autopkgtest-reboot"
	autopkgtestRebootScript = `#!/bin/bash
set -euo pipefail
~core/kolet reboot-request $1
reboot
`
	autopkgTestRebootPreparePath = "/tmp/autopkgtest-reboot-prepare"

	autopkgtestRebootPrepareScript = `#!/bin/bash
set -euo pipefail
exec ~core/kolet reboot-request $1
`

	// File used to communicate between the script and the kolet runner internally
	rebootStamp = "/run/kolet-reboot"
)

var (
	plog = capnslog.NewPackageLogger("github.com/coreos/mantle", "kolet")

	root = &cobra.Command{
		Use:   "kolet run [test] [func]",
		Short: "Native code runner for kola",
		Run:   run,
	}

	cmdRun = &cobra.Command{
		Use:   "run [test] [func]",
		Short: "Run a given test's native function",
		Run:   run,
	}

	cmdRunExtUnit = &cobra.Command{
		Use:          "run-test-unit [unitname]",
		Short:        "Monitor execution of a systemd unit",
		RunE:         runExtUnit,
		SilenceUsage: true,
	}

	cmdReboot = &cobra.Command{
		Use:          "reboot-request MARK",
		Short:        "Request a reboot",
		RunE:         runReboot,
		SilenceUsage: true,
	}
)

func run(cmd *cobra.Command, args []string) {
	cmd.Usage()
	os.Exit(2)
}

func registerTestMap(m map[string]*register.Test) {
	for testName, testObj := range m {
		if len(testObj.NativeFuncs) == 0 {
			continue
		}
		testCmd := &cobra.Command{
			Use: testName + " [func]",
			Run: run,
		}
		for nativeName := range testObj.NativeFuncs {
			nativeFuncWrap := testObj.NativeFuncs[nativeName]
			nativeRun := func(cmd *cobra.Command, args []string) {
				if len(args) != 0 {
					cmd.Usage()
					os.Exit(2)
				}
				if err := nativeFuncWrap.NativeFunc(); err != nil {
					plog.Fatal(err)
				}
				// Explicitly exit successfully.
				os.Exit(0)
			}
			nativeCmd := &cobra.Command{
				Use: nativeName,
				Run: nativeRun,
			}
			testCmd.AddCommand(nativeCmd)
		}
		cmdRun.AddCommand(testCmd)
	}
}

// dispatchRunExtUnit returns true if unit completed successfully, false if
// it's still running (or unit was terminated by SIGTERM)
func dispatchRunExtUnit(unitname string, sdconn *systemddbus.Conn) (bool, error) {
	props, err := sdconn.GetAllProperties(unitname)
	if err != nil {
		return false, errors.Wrapf(err, "listing unit properties")
	}

	result := props["Result"]
	if result == "exit-code" {
		return false, fmt.Errorf("Unit %s exited with code %d", unitname, props["ExecMainStatus"])
	}

	state := props["ActiveState"]
	substate := props["SubState"]

	switch state {
	case "inactive":
		sdconn.StartUnit(unitname, "fail", nil)
		return false, nil
	case "activating":
		return false, nil
	case "active":
		{
			switch substate {
			case "exited":
				maincode := props["ExecMainCode"]
				mainstatus := props["ExecMainStatus"]
				switch maincode {
				case CLD_EXITED:
					if mainstatus == int32(0) {
						return true, nil
					} else {
						// I don't think this can happen, we'd have exit-code above; but just
						// for completeness
						return false, fmt.Errorf("Unit %s failed with code %d", unitname, mainstatus)
					}
				case CLD_KILLED:
					return true, fmt.Errorf("Unit %s killed by signal %d", unitname, mainstatus)
				default:
					return false, fmt.Errorf("Unit %s had unhandled code %d", unitname, maincode)
				}
			case "running":
				return false, nil
			case "failed":
				return true, fmt.Errorf("Unit %s in substate 'failed'", unitname)
			default:
				// Pass through other states
				return false, nil
			}
		}
	default:
		return false, fmt.Errorf("Unhandled systemd unit state:%s", state)
	}
}

func initiateReboot() error {
	contents, err := ioutil.ReadFile(rebootStamp)
	if err != nil {
		return err
	}
	res := kola.KoletResult{
		Reboot: string(contents),
	}
	buf, err := json.Marshal(&res)
	if err != nil {
		return errors.Wrapf(err, "serializing KoletResult")
	}
	fmt.Println(string(buf))
	systemdjournal.Print(systemdjournal.PriInfo, "Acknowledged reboot request with mark: %s", buf)
	return nil
}

func runExtUnit(cmd *cobra.Command, args []string) error {
	// Write the autopkgtest wrappers
	if err := ioutil.WriteFile(autopkgTestRebootPath, []byte(autopkgtestRebootScript), 0755); err != nil {
		return err
	}
	if err := ioutil.WriteFile(autopkgTestRebootPreparePath, []byte(autopkgtestRebootPrepareScript), 0755); err != nil {
		return err
	}

	unitname := args[0]
	// Restrict this to services, don't need to support anything else right now
	if !strings.HasSuffix(unitname, ".service") {
		unitname = unitname + ".service"
	}
	sdconn, err := systemddbus.NewSystemConnection()
	if err != nil {
		return errors.Wrapf(err, "systemd connection")
	}

	// Start the unit; it's not started by default because we need to
	// do some preparatory work above (and some is done in the harness)
	if _, err := sdconn.StartUnit(unitname, "fail", nil); err != nil {
		return errors.Wrapf(err, "starting unit")
	}

	if err := sdconn.Subscribe(); err != nil {
		return err
	}
	dispatchRunExtUnit(unitname, sdconn)
	unitevents, uniterrs := sdconn.SubscribeUnits(time.Second)

	for {
		select {
		case m := <-unitevents:
			for n := range m {
				if n == unitname {
					r, err := dispatchRunExtUnit(unitname, sdconn)
					if err != nil {
						return err
					}
					if r {
						return nil
					}
				} else if n == kola.KoletRebootWaitUnit {
					return initiateReboot()
				}
			}
		case m := <-uniterrs:
			return m
		}
	}
}

// This is a backend intending to support at least the same
// API as defined by Debian autopkgtests:
// https://salsa.debian.org/ci-team/autopkgtest/raw/master/doc/README.package-tests.rst
func runReboot(cmd *cobra.Command, args []string) error {
	mark := args[0]
	systemdjournal.Print(systemdjournal.PriInfo, "Requesting reboot with mark: %s", mark)
	err := ioutil.WriteFile(rebootStamp, []byte(mark), 0644)
	if err != nil {
		return err
	}
	// Synchronously wait until the mark is propagated back to the harness
	err = exec.Command("systemd-run", "-q", "--wait", "--unit", kola.KoletRebootWaitUnit, "--", "sleep", "infinity").Run()
	if err != nil {
		return errors.Wrapf(err, "starting %s", kola.KoletRebootWaitUnit)
	}
	systemdjournal.Print(systemdjournal.PriInfo, "Reboot request acknowledged")
	return nil
}

func main() {
	registerTestMap(register.Tests)
	registerTestMap(register.UpgradeTests)
	root.AddCommand(cmdRun)
	root.AddCommand(cmdRunExtUnit)
	cmdReboot.Args = cobra.ExactArgs(1)
	root.AddCommand(cmdReboot)

	cli.Execute(root)
}
