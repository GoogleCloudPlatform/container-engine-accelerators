// Copyright 2017 Google Inc. All Rights Reserved.
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

package mig

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia/nvmlutil"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

func TestDiscoverGPUPartitions(t *testing.T) {
	testDevDir, err := ioutil.TempDir("", "dev")
	defer os.RemoveAll(testDevDir)
	if err != nil {
		t.Fatalf("failed to create temp dev dir: %v", err)
	}

	testProcDir, err := ioutil.TempDir("", "proc")
	defer os.RemoveAll(testProcDir)
	if err != nil {
		t.Fatalf("failed to create temp proc dir: %v", err)
	}

	if err := os.MkdirAll(path.Join(testProcDir, "driver/nvidia/capabilities/gpu0/mig/gi1/ci0"), 0755); err != nil {
		t.Fatalf("failed to create capabilities dir: %v", err)
	}
	if err := os.MkdirAll(path.Join(testProcDir, "driver/nvidia/capabilities/gpu0/mig/gi2/ci0"), 0755); err != nil {
		t.Fatalf("failed to create capabilities dir: %v", err)
	}

	err = os.MkdirAll(path.Join(testDevDir, "nvidia-caps"), 0755)
	if err != nil {
		t.Fatalf("failed to create capabilities device dir: %v", err)
	}

	// Create GI and CI acceess files
	capToMinorDevices := map[string]int{
		"driver/nvidia/capabilities/gpu0/mig/gi1/access":     12,
		"driver/nvidia/capabilities/gpu0/mig/gi1/ci0/access": 13,
		"driver/nvidia/capabilities/gpu0/mig/gi2/access":     21,
		"driver/nvidia/capabilities/gpu0/mig/gi2/ci0/access": 22,
	}
	for file, minor := range capToMinorDevices {
		if err := ioutil.WriteFile(path.Join(testProcDir, file), []byte(fmt.Sprintf("DeviceFileMinor: %d\nDeviceFileMode: 292", minor)), 0644); err != nil {
			t.Fatalf("failed to create proc capabilities file (%s): %v", file, err)
		}
	}

	// Create device nodes
	deviceNodes := []string{
		"nvidia-uvm",
		"nvidia-uvm-tools",
		"nvidiactl",
		"nvidia0",
		"nvidia-caps/nvidia-cap12",
		"nvidia-caps/nvidia-cap13",
		"nvidia-caps/nvidia-cap21",
		"nvidia-caps/nvidia-cap22",
	}
	for _, device := range deviceNodes {
		if _, err := os.Create(path.Join(testDevDir, device)); err != nil {
			t.Fatalf("failed to create device node (%s): %v", device, err)
		}
	}

	// overriding nvmlutil.NvmlDeviceInfo to nvmlutil.MockDeviceInfo interface
	nvmlutil.NvmlDeviceInfo = &nvmlutil.MockDeviceInfo{}

	deviceManager := NewDeviceManager(testDevDir, testProcDir)
	if err := deviceManager.Start("3g.20gb"); err != nil {
		t.Errorf("Mig device manager failed to start: %v", err)
	}

	devices := deviceManager.ListGPUPartitionDevices()
	if len(devices) != 2 {
		t.Errorf("incorrect number of GPU partitions. got = %d, want = %d", len(devices), 2)
	}

	expectedDevices := map[string][]pluginapi.DeviceSpec{
		"nvidia0/gi1": {
			{
				ContainerPath: path.Join(testDevDir, "nvidia0"),
				HostPath:      path.Join(testDevDir, "nvidia0"),
				Permissions:   "mrw",
			},
			{
				ContainerPath: path.Join(testDevDir, "nvidia-caps/nvidia-cap12"),
				HostPath:      path.Join(testDevDir, "nvidia-caps/nvidia-cap12"),
				Permissions:   "mrw",
			},
			{
				ContainerPath: path.Join(testDevDir, "nvidia-caps/nvidia-cap13"),
				HostPath:      path.Join(testDevDir, "nvidia-caps/nvidia-cap13"),
				Permissions:   "mrw",
			},
		},
		"nvidia0/gi2": {
			{
				ContainerPath: path.Join(testDevDir, "nvidia0"),
				HostPath:      path.Join(testDevDir, "nvidia0"),
				Permissions:   "mrw",
			},
			{
				ContainerPath: path.Join(testDevDir, "nvidia-caps/nvidia-cap21"),
				HostPath:      path.Join(testDevDir, "nvidia-caps/nvidia-cap21"),
				Permissions:   "mrw",
			},
			{
				ContainerPath: path.Join(testDevDir, "nvidia-caps/nvidia-cap22"),
				HostPath:      path.Join(testDevDir, "nvidia-caps/nvidia-cap22"),
				Permissions:   "mrw",
			},
		},
	}

	for id, specWant := range expectedDevices {
		_, ok := devices[id]
		if !ok {
			t.Errorf("device id %s not found", id)
		}

		specGot, err := deviceManager.DeviceSpec(id)
		if err != nil {
			t.Errorf("failed to look up device spec for %s", id)
		}

		if !reflect.DeepEqual(specGot, specWant) {
			t.Errorf("device specs for device %s do not match. got: %v, want %v", id, specGot, specWant)
		}
	}
}

type giSetup struct {
	name    string
	giMinor int
	ciMinor int
}

func setupMIGGPU(t *testing.T, devDir, procDir, gpuID string, gis []giSetup) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(devDir, "nvidia-caps"), 0755); err != nil {
		t.Fatalf("mkdir nvidia-caps: %v", err)
	}
	if err := os.WriteFile(filepath.Join(devDir, "nvidia"+gpuID), nil, 0644); err != nil {
		t.Fatalf("create nvidia%s: %v", gpuID, err)
	}
	migDir := filepath.Join(procDir, "driver/nvidia/capabilities", "gpu"+gpuID, "mig")
	if err := os.RemoveAll(migDir); err != nil {
		t.Fatalf("clean %s: %v", migDir, err)
	}
	if err := os.MkdirAll(migDir, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", migDir, err)
	}
	for _, g := range gis {
		ciDir := filepath.Join(migDir, g.name, "ci0")
		if err := os.MkdirAll(ciDir, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", ciDir, err)
		}
		if err := os.WriteFile(filepath.Join(migDir, g.name, "access"), []byte(fmt.Sprintf("DeviceFileMinor: %d\n", g.giMinor)), 0644); err != nil {
			t.Fatalf("write gi access: %v", err)
		}
		if err := os.WriteFile(filepath.Join(ciDir, "access"), []byte(fmt.Sprintf("DeviceFileMinor: %d\n", g.ciMinor)), 0644); err != nil {
			t.Fatalf("write ci access: %v", err)
		}
		for _, minor := range []int{g.giMinor, g.ciMinor} {
			if err := os.WriteFile(filepath.Join(devDir, "nvidia-caps", fmt.Sprintf("nvidia-cap%d", minor)), nil, 0644); err != nil {
				t.Fatalf("create cap %d: %v", minor, err)
			}
		}
	}
}

func TestStartAcceptsDifferentGINumbersAndRejectsMisconfiguredNode(t *testing.T) {
	nvmlutil.NvmlDeviceInfo = &nvmlutil.MockDeviceInfo{}

	t.Run("each GPU matches partition size with different GI numbers", func(t *testing.T) {
		devDir := t.TempDir()
		procDir := t.TempDir()
		setupMIGGPU(t, devDir, procDir, "0", []giSetup{
			{name: "gi1", giMinor: 11, ciMinor: 12},
			{name: "gi2", giMinor: 21, ciMinor: 22},
		})
		setupMIGGPU(t, devDir, procDir, "1", []giSetup{
			{name: "gi5", giMinor: 51, ciMinor: 52},
			{name: "gi6", giMinor: 61, ciMinor: 62},
		})
		dm := NewDeviceManager(devDir, procDir)
		if err := dm.Start("3g.20gb"); err != nil {
			t.Fatalf("Start failed when each GPU matches 3g.20gb: %v", err)
		}
		if got := dm.PartitionIDs("nvidia0"); !reflect.DeepEqual(got, []string{"nvidia0/gi1", "nvidia0/gi2"}) {
			t.Errorf("PartitionIDs(nvidia0) = %v, want [nvidia0/gi1 nvidia0/gi2]", got)
		}
		if got := dm.PartitionIDs("nvidia1"); !reflect.DeepEqual(got, []string{"nvidia1/gi5", "nvidia1/gi6"}) {
			t.Errorf("PartitionIDs(nvidia1) = %v, want [nvidia1/gi5 nvidia1/gi6]", got)
		}
	})

	t.Run("rejects node when one GPU has wrong partition count", func(t *testing.T) {
		devDir := t.TempDir()
		procDir := t.TempDir()
		setupMIGGPU(t, devDir, procDir, "0", []giSetup{
			{name: "gi1", giMinor: 11, ciMinor: 12},
			{name: "gi2", giMinor: 21, ciMinor: 22},
		})
		setupMIGGPU(t, devDir, procDir, "1", []giSetup{
			{name: "gi1", giMinor: 31, ciMinor: 32},
		})
		dm := NewDeviceManager(devDir, procDir)
		if err := dm.Start("3g.20gb"); err == nil {
			t.Fatal("Start succeeded, want error when GPU 1 has only 1 partition")
		}
	})

	t.Run("rejects node when a physical GPU in /dev has no MIG capabilities", func(t *testing.T) {
		devDir := t.TempDir()
		procDir := t.TempDir()
		setupMIGGPU(t, devDir, procDir, "0", []giSetup{
			{name: "gi1", giMinor: 11, ciMinor: 12},
			{name: "gi2", giMinor: 21, ciMinor: 22},
		})
		if err := os.WriteFile(filepath.Join(devDir, "nvidia1"), nil, 0644); err != nil {
			t.Fatalf("create nvidia1: %v", err)
		}
		dm := NewDeviceManager(devDir, procDir)
		if err := dm.Start("3g.20gb"); err == nil {
			t.Fatal("Start succeeded, want error when nvidia1 is unpartitioned")
		}
	})
}

func TestDiscoverGPUPerGPUIsolation(t *testing.T) {
	nvmlutil.NvmlDeviceInfo = &nvmlutil.MockDeviceInfo{}
	devDir := t.TempDir()
	procDir := t.TempDir()

	setupMIGGPU(t, devDir, procDir, "0", []giSetup{
		{name: "gi1", giMinor: 11, ciMinor: 12},
		{name: "gi2", giMinor: 21, ciMinor: 22},
	})
	setupMIGGPU(t, devDir, procDir, "1", []giSetup{
		{name: "gi1", giMinor: 31, ciMinor: 32},
		{name: "gi2", giMinor: 41, ciMinor: 42},
	})

	dm := NewDeviceManager(devDir, procDir)
	if err := dm.Start("3g.20gb"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	dm.SetDeviceHealth("nvidia1/gi1", pluginapi.Unhealthy, nil)

	// Re-partition GPU 0 with new GI names and cap minors.
	setupMIGGPU(t, devDir, procDir, "0", []giSetup{
		{name: "gi3", giMinor: 71, ciMinor: 72},
		{name: "gi4", giMinor: 81, ciMinor: 82},
	})
	if err := dm.DiscoverGPU("nvidia0", "3g.20gb"); err != nil {
		t.Fatalf("DiscoverGPU(nvidia0): %v", err)
	}

	if got := dm.PartitionIDs("nvidia0"); !reflect.DeepEqual(got, []string{"nvidia0/gi3", "nvidia0/gi4"}) {
		t.Errorf("PartitionIDs(nvidia0) = %v, want [nvidia0/gi3 nvidia0/gi4]", got)
	}
	if got := dm.PartitionIDs("nvidia1"); !reflect.DeepEqual(got, []string{"nvidia1/gi1", "nvidia1/gi2"}) {
		t.Errorf("PartitionIDs(nvidia1) = %v, want [nvidia1/gi1 nvidia1/gi2]", got)
	}
	if got := dm.ListGPUPartitionDevices()["nvidia1/gi1"].Health; got != pluginapi.Unhealthy {
		t.Errorf("nvidia1/gi1 health = %q, want %q", got, pluginapi.Unhealthy)
	}
}

func TestPartitionerPerGPUAndInUseErrors(t *testing.T) {
	ctx := context.Background()
	var cmds []string
	smi := func(ctx context.Context, args ...string) ([]byte, error) {
		cmd := strings.Join(args, " ")
		cmds = append(cmds, cmd)
		if cmd == "mig -lgi -i 0000:8f:00.0" {
			return []byte("No GPU instances found\n"), nil
		}
		return nil, nil
	}

	if err := ConvergePartitions(ctx, smi, "0000:8f:00.0", "3g.20gb"); err != nil {
		t.Fatalf("ConvergePartitions failed: %v", err)
	}
	want := []string{
		"mig -lgi -i 0000:8f:00.0",
		"mig -dci -i 0000:8f:00.0",
		"mig -dgi -i 0000:8f:00.0",
		"mig -cgi 9,9 -i 0000:8f:00.0",
		"mig -cci -i 0000:8f:00.0",
	}
	if !reflect.DeepEqual(cmds, want) {
		t.Errorf("ConvergePartitions cmds = %v, want %v", cmds, want)
	}

	// Verify IsGPUPartitioned and CheckCurrentPartitionProfileCounts succeed when
	// scoped by PCI bus ID even when column 0 of -lgi is NVML index 0 (not minor 2).
	lgiColumn0Zero := func(ctx context.Context, args ...string) ([]byte, error) {
		return []byte(`
+-----------------------------------------------------------------------------+
| GPU instances:                                                              |
| GPU   Profile Name   Profile ID   CI_ID   Address                           |
|=============================================================================|
|   0   MIG 3g.20gb    9            1       00000000                          |
|   0   MIG 3g.20gb    9            2       00000001                          |
+-----------------------------------------------------------------------------+
`), nil
	}
	if !IsGPUPartitioned(ctx, lgiColumn0Zero, "0000:8f:00.0", "3g.20gb") {
		t.Error("IsGPUPartitioned returned false for PCI-scoped -lgi output with NVML index 0")
	}
	if !CheckCurrentPartitionProfileCounts(ctx, lgiColumn0Zero, "0000:8f:00.0", 2) {
		t.Error("CheckCurrentPartitionProfileCounts returned false for PCI-scoped -lgi output with NVML index 0")
	}

	inUseSMI := func(ctx context.Context, args ...string) ([]byte, error) {
		return []byte("Unable to destroy GPU instance ID 1 on GPU 2: In use by another client"), errors.New("exit status 1")
	}
	err := DestroyPartitions(ctx, inUseSMI, "0000:8f:00.0")
	var inUseErr *InstanceInUseError
	if !errors.As(err, &inUseErr) {
		t.Fatalf("DestroyPartitions error = %T (%v), want *InstanceInUseError", err, err)
	}
	if inUseErr.Instance != "nvidia2/gi1" {
		t.Errorf("InstanceInUseError.Instance = %q, want %q", inUseErr.Instance, "nvidia2/gi1")
	}
}
