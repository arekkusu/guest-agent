//  Copyright 2024 Google LLC
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.

// Package manager is responsible for detecting the current network manager service, and
// writing and rolling back appropriate configurations for each network manager service.
package manager

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path"
	"slices"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/guest-agent/google_guest_agent/run"
	"github.com/GoogleCloudPlatform/guest-agent/metadata"
	"github.com/go-ini/ini"
)

// mockSystemd is the test systemd-networkd implementation to use for testing.
var (
	mockSystemd = systemdNetworkd{
		networkCtlKeys: []string{"AdministrativeState", "SetupState"},
		priority:       1,
	}
)

// systemdTestOpts is a wrapper for all options to set for test setup.
type systemdTestOpts struct {
	// lookPathOpts contains options for lookPath mocking.
	lookPathOpts systemdLookPathOpts

	// runnerOpts contains options for run mocking.
	runnerOpts systemdRunnerOpts
}

// systemdLookPathOpts contains options for lookPath mocking.
type systemdLookPathOpts struct {
	// returnErr indicates whether to return error.
	returnErr bool

	// returnValue indicates the return value for mocking.
	returnValue bool
}

// systemdMockRunner is the Mock Runner to use for testing.
type systemdMockRunner struct {
	// versionOpts are options for when running `networkctl --version`
	versionOpts systemdVersionOpts

	// isActiveErr is an option for running `systemctl is-active systemd-networkd.service`
	// isActiveErr indicates whether to return an error when running the command.
	isActiveErr bool

	// statusOpts are options for running `networkctl status iface --json=short`
	statusOpts systemdStatusOpts
}

// systemdVersionOpts are options for running `networkctl --version`.
type systemdVersionOpts struct {
	// returnErr indicates whether the command should return an error.
	returnErr bool

	// version indicates the version to return when running the command.
	version int
}

// systemdStatusOpts are options for running `networkctl status iface --json=short`
type systemdStatusOpts struct {
	// returnValue indicates whether to return a configured or non-configured interface.
	returnValue bool

	// returnErr indicates whether to return an error.
	returnErr bool

	// hasKey determines whether the configuredKey should be included or not.
	hasKey bool

	// configuredKey is used only when returnValue is not err. This indicates what key to
	// use for determining the configured state.
	configuredKey string
}

// systemdRunnerOpts are options to set for intializing the MockRunner.
type systemdRunnerOpts struct {
	// versionOpts are options for when running `networkctl --version`
	versionOpts systemdVersionOpts

	// isActiveErr is an option for running `systemctl is-active systemd-networkd.service`
	// isActiveErr indicates whether to return an error when running the command.
	isActiveErr bool

	// statusOpts are options for running `networkctl status iface --json=short`
	statusOpts systemdStatusOpts
}

func (s systemdMockRunner) Quiet(ctx context.Context, name string, args ...string) error {
	// The systemd-networkd implementation only uses Quiet for reloading configurations, so skip
	// that call.
	return nil
}

func (s systemdMockRunner) WithOutput(ctx context.Context, name string, args ...string) *run.Result {
	if name == "networkctl" && slices.Contains(args, "--version") {
		verOpts := s.versionOpts
		if verOpts.returnErr {
			return &run.Result{
				ExitCode: 1,
				StdErr:   "mock error version",
			}
		}
		return &run.Result{
			StdOut: fmt.Sprintf("systemd %v (%v-1.0)\n+TEST +ESTT +STTE +TTES", verOpts.version, verOpts.version),
		}
	}
	if name == "systemctl" && slices.Contains(args, "is-active") && slices.Contains(args, "systemd-networkd.service") {
		if s.isActiveErr {
			return &run.Result{
				ExitCode: 1,
			}
		}
		return &run.Result{}
	}
	if name == "/bin/sh" && slices.Contains(args, "networkctl status iface --json=short") {
		statusOpts := s.statusOpts

		if statusOpts.returnErr {
			return &run.Result{
				ExitCode: 1,
				StdErr:   "mock error status",
			}
		}
		if statusOpts.returnValue {
			mockOut := fmt.Sprintf(`{"Name": "iface", "%s": "%s"}`, statusOpts.configuredKey, "configured")
			return &run.Result{
				StdOut: mockOut,
			}
		}

		if statusOpts.hasKey {
			mockOut := fmt.Sprintf(`{"Name": "iface", "%s": "%s"}`, statusOpts.configuredKey, "unmanaged")
			return &run.Result{
				StdOut: mockOut,
			}
		}
		mockOut := fmt.Sprintf("{\"Name\": \"iface\"}")
		return &run.Result{
			StdOut: mockOut,
		}
	}
	return &run.Result{
		ExitCode: 1,
		StdErr:   "unexpected command",
	}
}

func (s systemdMockRunner) WithOutputTimeout(ctx context.Context, timeout time.Duration, name string, args ...string) *run.Result {
	return &run.Result{}
}

func (s systemdMockRunner) WithCombinedOutput(ctx context.Context, name string, args ...string) *run.Result {
	return &run.Result{}
}

// systemdTestSetup sets up the environment before each test.
func systemdTestSetup(t *testing.T, opts systemdTestOpts) {
	t.Helper()
	mockDir := path.Join(t.TempDir(), "systemd", "network")
	mockSystemd.configDir = mockDir

	runnerOpts := opts.runnerOpts
	lookPathOpts := opts.lookPathOpts

	// Create the temporary directory.
	if err := os.MkdirAll(mockDir, 0755); err != nil {
		t.Fatalf("failed to create mock network config directory: %v", err)
	}

	if lookPathOpts.returnErr {
		execLookPath = func(name string) (string, error) {
			return "", fmt.Errorf("mock error finding path")
		}
	} else if lookPathOpts.returnValue {
		execLookPath = func(name string) (string, error) {
			return name, nil
		}
	} else {
		execLookPath = func(name string) (string, error) {
			return "", exec.ErrNotFound
		}
	}

	run.Client = &systemdMockRunner{
		versionOpts: runnerOpts.versionOpts,
		isActiveErr: runnerOpts.isActiveErr,
		statusOpts:  runnerOpts.statusOpts,
	}
}

// systemdTestTearDown cleans up after each test.
func systemdTestTearDown(t *testing.T) {
	t.Helper()

	execLookPath = exec.LookPath
	run.Client = &run.Runner{}
}

// TestSystemdNetworkdIsManaging tests whether IsManaging behaves correctly given some
// mock environment setup.
func TestSystemdNetworkdIsManaging(t *testing.T) {
	tests := []struct {
		// name is the name of the test.
		name string

		// opts are the options to set for test environment setup.
		opts systemdTestOpts

		// expectedRes is the expected return value of IsManaging()
		expectedRes bool

		// expectErr determines whether an error is expected.
		expectErr bool

		// expectedErr is the expected error message.
		expectedErr string
	}{
		// networkctl does not exist.
		{
			name: "no-networkctl",
			opts: systemdTestOpts{
				lookPathOpts: systemdLookPathOpts{
					returnValue: false,
				},
			},
			expectedRes: false,
			expectErr:   false,
		},
		// LookPath error.
		{
			name: "lookpath-error",
			opts: systemdTestOpts{
				lookPathOpts: systemdLookPathOpts{
					returnErr: true,
				},
			},
			expectedRes: false,
			expectErr:   true,
			expectedErr: "error looking up networkctl path: mock error finding path",
		},
		// networkctl unsupported version
		{
			name: "unsupported-systemd-version",
			opts: systemdTestOpts{
				lookPathOpts: systemdLookPathOpts{
					returnValue: true,
				},
				runnerOpts: systemdRunnerOpts{
					versionOpts: systemdVersionOpts{
						version: 249,
					},
				},
			},
			expectedRes: false,
			expectErr:   false,
		},
		// networkctl version error
		{
			name: "systemd-version-error",
			opts: systemdTestOpts{
				lookPathOpts: systemdLookPathOpts{
					returnValue: true,
				},
				runnerOpts: systemdRunnerOpts{
					versionOpts: systemdVersionOpts{
						returnErr: true,
					},
				},
			},
			expectedRes: false,
			expectErr:   true,
			expectedErr: "error checking networkctl version: mock error version",
		},
		// networkctl is-active error.
		{
			name: "networkctl-is-active-error",
			opts: systemdTestOpts{
				lookPathOpts: systemdLookPathOpts{
					returnValue: true,
				},
				runnerOpts: systemdRunnerOpts{
					versionOpts: systemdVersionOpts{
						version: 300,
					},
					isActiveErr: true,
				},
			},
			expectedRes: false,
			expectErr:   false,
		},
		// networkctl status error.
		{
			name: "networkctl-status-error",
			opts: systemdTestOpts{
				lookPathOpts: systemdLookPathOpts{
					returnValue: true,
				},
				runnerOpts: systemdRunnerOpts{
					versionOpts: systemdVersionOpts{
						version: 300,
					},
					statusOpts: systemdStatusOpts{
						returnErr: true,
					},
				},
			},
			expectedRes: false,
			expectErr:   true,
			expectedErr: "failed to check systemd-networkd network status: mock error status",
		},
		// networkctl status no networkctl key.
		{
			name: "networkctl-status-no-key",
			opts: systemdTestOpts{
				lookPathOpts: systemdLookPathOpts{
					returnValue: true,
				},
				runnerOpts: systemdRunnerOpts{
					versionOpts: systemdVersionOpts{
						version: 300,
					},
					statusOpts: systemdStatusOpts{
						returnValue: false,
						hasKey:      false,
					},
				},
			},
			expectedRes: false,
			expectErr:   true,
			expectedErr: "could not determine interface state, one of [AdministrativeState SetupState] was not present",
		},
		// networkctl status interface is unmanaged.
		{
			name: "networkctl-status-unmanaged",
			opts: systemdTestOpts{
				lookPathOpts: systemdLookPathOpts{
					returnValue: true,
				},
				runnerOpts: systemdRunnerOpts{
					versionOpts: systemdVersionOpts{
						version: 300,
					},
					statusOpts: systemdStatusOpts{
						returnValue:   false,
						hasKey:        true,
						configuredKey: "AdministrativeState",
					},
				},
			},
			expectedRes: false,
			expectErr:   false,
		},
		// networkctl status interface is managed. Whole method passes.
		{
			name: "pass",
			opts: systemdTestOpts{
				lookPathOpts: systemdLookPathOpts{
					returnValue: true,
				},
				runnerOpts: systemdRunnerOpts{
					versionOpts: systemdVersionOpts{
						version: 300,
					},
					statusOpts: systemdStatusOpts{
						returnValue:   true,
						hasKey:        true,
						configuredKey: "SetupState",
					},
				},
			},
			expectedRes: true,
			expectErr:   false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			systemdTestSetup(t, test.opts)

			res, err := mockSystemd.IsManaging(ctx, "iface")

			// Check expected errors.
			if err != nil && !test.expectErr {
				t.Fatalf("err returned when none expected: %v", err)
			}
			if test.expectErr {
				if err == nil {
					t.Fatalf("no err returned when err expected")
				}
				if err.Error() != test.expectedErr {
					t.Fatalf("mismatched error message.\nExpected: %v\nActual: %v\n", test.expectedErr, err)
				}
			}

			// Check expected output.
			if res != test.expectedRes {
				t.Fatalf("incorrect return value. Expected: %v, Actual: %v", test.expectedRes, res)
			}

			dhclientTestTearDown(t)
		})
	}
}

// TestSystemdNetworkdConfig tests whether config file writing works correctly.
func TestSystemdNetworkdConfig(t *testing.T) {
	tests := []struct {
		// name is the name of the test.
		name string

		// testInterfaces is the list of mock interfaces.
		testInterfaces []string

		// testIpv6Interfaces is the list of mock IPv6 interfaces.
		testIpv6Interfaces []string

		// expectedFiles is the list of expected file names.
		expectedFiles []string

		// expectedDHCP is the list of expected DHCP values.
		expectedDHCP []string
	}{
		{
			name:           "ipv4",
			testInterfaces: []string{"iface0"},
			expectedFiles: []string{
				"1-iface0-google-guest-agent.network",
			},
			expectedDHCP: []string{
				"ipv4",
			},
		},
		{
			name:               "ipv6",
			testInterfaces:     []string{"iface0"},
			testIpv6Interfaces: []string{"iface0"},
			expectedFiles: []string{
				"1-iface0-google-guest-agent.network",
			},
			expectedDHCP: []string{
				"yes",
			},
		},
		{
			name:               "multinic",
			testInterfaces:     []string{"iface0", "iface1"},
			testIpv6Interfaces: []string{"iface1"},
			expectedFiles: []string{
				"1-iface0-google-guest-agent.network",
				"1-iface1-google-guest-agent.network",
			},
			expectedDHCP: []string{
				"ipv4",
				"yes",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			systemdTestSetup(t, systemdTestOpts{})

			if err := mockSystemd.writeEthernetConfig(test.testInterfaces, test.testIpv6Interfaces); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Check the files.
			files, err := os.ReadDir(mockSystemd.configDir)
			if err != nil {
				t.Fatalf("error reading configuration directory: %v", err)
			}

			for i, file := range files {
				// Ensure the only files are those written by guest agent.
				if !slices.Contains(test.expectedFiles, file.Name()) {
					t.Fatalf("unexpected file in configuration directory: %v", file.Name())
				}

				// Check contents.
				filePath := path.Join(mockSystemd.configDir, file.Name())
				opts := ini.LoadOptions{
					Loose:       true,
					Insensitive: true,
				}

				config, err := ini.LoadSources(opts, filePath)
				if err != nil {
					t.Fatalf("error loading config file: %v", err)
				}

				sections := new(systemdConfig)
				if err := config.MapTo(sections); err != nil {
					t.Fatalf("error parsing config ini: %v", err)
				}

				// Check for the GuestAgent section.
				if !sections.GuestAgent.Managed {
					t.Errorf("%s missing guest agent section", file.Name())
				}

				// Check that the file matches the interface.
				if sections.Match.Name != test.testInterfaces[i] {
					t.Errorf(`%s does not have correct match.
						Expected: %s
						Actual: %s`, file.Name(), test.testInterfaces[i], sections.Match.Name)
				}

				// Make sure the DHCP section is set correctly.
				if sections.Network.DHCP != test.expectedDHCP[i] {
					t.Errorf(`%s has incorrect DHCP value.
						Expected: %s
						Actual: %s`, file.Name(), test.expectedDHCP[i], sections.Network.DHCP)
				}

				// For non-primary interfaces, check DNSDefaultRoute field.
				if i != 0 {
					if sections.Network.DNSDefaultRoute {
						t.Errorf("%s, a secondary interface, has DNSDefaultRoute set", file.Name())
					}
				}
			}
			// Cleanup.
			systemdTestTearDown(t)
		})
	}
}

func TestSetupVlanInterfaceSuccess(t *testing.T) {
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatalf("could not list local interfaces: %+v", err)
	}

	tests := []struct {
		ethernetInterface metadata.NetworkInterfaces
		vlanInterface     metadata.VlanInterface
	}{
		{
			vlanInterface: metadata.VlanInterface{
				Mac:             "foobar",
				ParentInterface: "/computeMetadata/v1/instance/network-interfaces/0/",
				Vlan:            22,
			},
			ethernetInterface: metadata.NetworkInterfaces{
				Mac: ifaces[1].HardwareAddr.String(),
			},
		},
		{
			vlanInterface: metadata.VlanInterface{
				Mac:             "foobar",
				ParentInterface: "/computeMetadata/v1/instance/network-interfaces/0/",
				Vlan:            33,
			},
			ethernetInterface: metadata.NetworkInterfaces{
				Mac: ifaces[1].HardwareAddr.String(),
			},
		},
	}

	opts := systemdTestOpts{
		lookPathOpts: systemdLookPathOpts{
			returnValue: true,
		},
		runnerOpts: systemdRunnerOpts{
			versionOpts: systemdVersionOpts{
				version: 300,
			},
			statusOpts: systemdStatusOpts{
				returnValue:   true,
				hasKey:        true,
				configuredKey: "SetupState",
			},
		}}

	for i, curr := range tests {
		t.Run(fmt.Sprintf("test-setup-vlan-succes-%d", i), func(t *testing.T) {
			testDir := t.TempDir()

			impl := &systemdNetworkd{
				configDir:      testDir,
				networkCtlKeys: []string{"AdministrativeState", "SetupState"},
				priority:       1,
			}

			systemdTestSetup(t, opts)

			t.Cleanup(func() {
				dhclientTestTearDown(t)
			})

			nics := &Interfaces{
				EthernetInterfaces: []metadata.NetworkInterfaces{curr.ethernetInterface},
				VlanInterfaces: map[int]metadata.VlanInterface{
					curr.vlanInterface.Vlan: curr.vlanInterface,
				},
			}

			ctx := context.Background()
			if err := impl.SetupVlanInterface(ctx, nil, nics); err != nil {
				t.Fatalf("expected err: nil, got: %+v", err)
			}

			networkFileName := fmt.Sprintf("1-gcp.%s.%d-google-guest-agent.network", ifaces[1].Name, curr.vlanInterface.Vlan)
			networkFile := path.Join(testDir, networkFileName)

			fileExists := func(fpath string, shouldExist bool) {
				t.Helper()
				_, err := os.Stat(fpath)
				if shouldExist && err != nil && os.IsNotExist(err) {
					t.Fatalf("expected to have file(%s), got error: %+v", fpath, err)
				} else if !shouldExist && err == nil {
					t.Fatalf("expected to not have file(%s), got error: nil", fpath)
				}
			}

			netdevFileName := fmt.Sprintf("1-gcp.%s.%d-google-guest-agent.netdev", ifaces[1].Name, curr.vlanInterface.Vlan)
			netdevFile := path.Join(testDir, netdevFileName)

			fileExists(networkFile, true)
			fileExists(netdevFile, true)

			// Trying to re install it should produce failure.
			if err := impl.SetupVlanInterface(ctx, nil, nics); err != nil {
				t.Fatalf("expected err: nil, got: %+v", err)
			}

			fileExists(networkFile, true)
			fileExists(netdevFile, true)

			// Running SetupVlanInterface() without VlanInterfaces do actually cleanup/remove
			// vlan configurations.
			nics.VlanInterfaces = nil
			if err := impl.SetupVlanInterface(ctx, nil, nics); err != nil {
				t.Fatalf("expected err: nil, got: %+v", err)
			}

			fileExists(networkFile, false)
			fileExists(netdevFile, false)
		})
	}
}

func TestSetupVlanInterfaceFailure(t *testing.T) {
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatalf("could not list local interfaces: %+v", err)
	}

	tests := []struct {
		ethernetInterface metadata.NetworkInterfaces
		vlanInterface     metadata.VlanInterface
	}{
		{
			vlanInterface: metadata.VlanInterface{
				Mac:             "foobar",
				ParentInterface: "/computeMetadata/v1/instance/network-interfaces/x/",
				Vlan:            33,
			},
			ethernetInterface: metadata.NetworkInterfaces{
				Mac: ifaces[1].HardwareAddr.String(),
			},
		},
		{
			vlanInterface: metadata.VlanInterface{
				Mac:             "foobar",
				ParentInterface: "/computeMetadata/v1/instance/network-interfaces/1/",
				Vlan:            11,
			},
			ethernetInterface: metadata.NetworkInterfaces{
				Mac: ifaces[1].HardwareAddr.String(),
			},
		},
		{
			vlanInterface: metadata.VlanInterface{
				Mac:             "foobar",
				ParentInterface: "/computeMetadata/v1/instance/network-interfaces/0/",
				Vlan:            22,
			},
			ethernetInterface: metadata.NetworkInterfaces{
				Mac: "foo-bar",
			},
		},
	}

	opts := systemdTestOpts{
		lookPathOpts: systemdLookPathOpts{
			returnValue: true,
		},
		runnerOpts: systemdRunnerOpts{
			versionOpts: systemdVersionOpts{
				version: 300,
			},
			statusOpts: systemdStatusOpts{
				returnValue:   true,
				hasKey:        true,
				configuredKey: "SetupState",
			},
		}}

	for i, curr := range tests {
		t.Run(fmt.Sprintf("test-setup-vlan-success-%d", i), func(t *testing.T) {
			impl := &systemdNetworkd{
				configDir:      t.TempDir(),
				networkCtlKeys: []string{"AdministrativeState", "SetupState"},
				priority:       1,
			}

			systemdTestSetup(t, opts)

			t.Cleanup(func() {
				dhclientTestTearDown(t)
			})

			nics := &Interfaces{
				EthernetInterfaces: []metadata.NetworkInterfaces{curr.ethernetInterface},
				VlanInterfaces: map[int]metadata.VlanInterface{
					curr.vlanInterface.Vlan: curr.vlanInterface,
				},
			}

			ctx := context.Background()
			if err := impl.SetupVlanInterface(ctx, nil, nics); err == nil {
				t.Fatal("expected err: non-nill, got: nil")
			}
		})
	}
}
