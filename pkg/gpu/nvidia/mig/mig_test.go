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
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"reflect"
	"testing"

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
