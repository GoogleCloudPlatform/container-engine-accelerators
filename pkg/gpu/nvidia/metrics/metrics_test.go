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

package metrics

import (
	"fmt"
	"testing"
	"time"

	"github.com/NVIDIA/gpu-monitoring-tools/bindings/go/nvml"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func uint64Ptr(u uint64) *uint64 {
	return &u
}

func stringPtr(u string) *string {
	return &u
}

type MockDevice struct {
	DeviceInfo *nvml.Device
	Status     *nvml.DeviceStatus
}

type mockDeviceWrapper struct {
	device MockDevice
}

func (d *mockDeviceWrapper) giveDevice() *nvml.Device {
	return d.device.DeviceInfo
}

func (d *mockDeviceWrapper) giveStatus() (status *nvml.DeviceStatus, err error) {
	status = d.device.Status
	return status, nil
}

type MockGather struct{}

func (t *MockGather) gatherDevice(deviceName string) (deviceWrapper, error) {
	device, ok := gpuDevicesMock[deviceName]
	if !ok {
		return &mockDeviceWrapper{}, fmt.Errorf("device %s not found", deviceName)
	}

	return &mockDeviceWrapper{*device}, nil
}

func (t *MockGather) gatherStatus(d deviceWrapper) (status *nvml.DeviceStatus, err error) {
	return d.giveStatus()
}

func (t *MockGather) gatherDutyCycle(uuid string, since time.Duration) (uint, error) {
	dutyCycle, ok := dutyCycleMock[uuid]
	if !ok {
		return 0, fmt.Errorf("duty cycle for %s not found", uuid)
	}
	return dutyCycle, nil
}

var (
	containerDevicesMock = map[ContainerID][]string{
		{
			namespace: "default",
			pod:       "pod1",
			container: "container1",
		}: {
			"q759757",
		},
		{
			namespace: "non-default",
			pod:       "pod2",
			container: "container2",
		}: {
			"afjodaj",
			"7v89zhi",
		},
	}

	gpuDevicesMock = map[string]*MockDevice{
		"q759757": {
			DeviceInfo: &nvml.Device{
				UUID:   "656547758",
				Model:  stringPtr("model1"),
				Memory: uint64Ptr(200),
			},
			Status: &nvml.DeviceStatus{
				Memory: nvml.MemoryInfo{
					Global: nvml.DeviceMemory{
						Used: uint64Ptr(50),
					},
				},
			},
		},
		"afjodaj": {
			DeviceInfo: &nvml.Device{
				UUID:   "850729563",
				Model:  stringPtr("model2"),
				Memory: uint64Ptr(200),
			},
			Status: &nvml.DeviceStatus{
				Memory: nvml.MemoryInfo{
					Global: nvml.DeviceMemory{
						Used: uint64Ptr(150),
					},
				},
			},
		},
		"7v89zhi": {
			DeviceInfo: &nvml.Device{
				UUID:   "3572375710",
				Model:  stringPtr("model1"),
				Memory: uint64Ptr(350),
			},
			Status: &nvml.DeviceStatus{
				Memory: nvml.MemoryInfo{
					Global: nvml.DeviceMemory{
						Used: uint64Ptr(100),
					},
				},
			},
		},
	}

	dutyCycleMock = map[string]uint{
		"656547758":  78,
		"850729563":  32,
		"3572375710": 13,
	}
)

func TestMetricsUpdate(t *testing.T) {
	g = &MockGather{}
	ms := MetricServer{}
	ms.updateMetrics(containerDevicesMock)

	if testutil.ToFloat64(
		AcceleratorRequests.WithLabelValues(
			"default", "pod1", "container1", gpuResourceName)) != 1 ||
		testutil.ToFloat64(
			AcceleratorRequests.WithLabelValues(
				"non-default", "pod2", "container2", gpuResourceName)) != 2 {
		t.Fatalf("Wrong Result in AcceleratorRequsets")
	}

	if testutil.ToFloat64(
		DutyCycle.WithLabelValues(
			"default", "pod1", "container1", "nvidia", "656547758", "model1")) != 78 ||
		testutil.ToFloat64(
			DutyCycle.WithLabelValues(
				"non-default", "pod2", "container2", "nvidia", "850729563", "model2")) != 32 ||
		testutil.ToFloat64(
			DutyCycle.WithLabelValues(
				"non-default", "pod2", "container2", "nvidia", "3572375710", "model1")) != 13 {
		t.Fatalf("Wrong Result in DutyCycle")
	}

	if testutil.ToFloat64(
		MemoryTotal.WithLabelValues(
			"default", "pod1", "container1", "nvidia", "656547758", "model1")) != 200*1024*1024 ||
		testutil.ToFloat64(
			MemoryTotal.WithLabelValues(
				"non-default", "pod2", "container2", "nvidia", "850729563", "model2")) != 200*1024*1024 ||
		testutil.ToFloat64(
			MemoryTotal.WithLabelValues(
				"non-default", "pod2", "container2", "nvidia", "3572375710", "model1")) != 350*1024*1024 {
		t.Fatalf("Wrong Result in MemoryTotal")
	}

	if testutil.ToFloat64(
		MemoryUsed.WithLabelValues(
			"default", "pod1", "container1", "nvidia", "656547758", "model1")) != 50*1024*1024 ||
		testutil.ToFloat64(
			MemoryUsed.WithLabelValues(
				"non-default", "pod2", "container2", "nvidia", "850729563", "model2")) != 150*1024*1024 ||
		testutil.ToFloat64(
			MemoryUsed.WithLabelValues(
				"non-default", "pod2", "container2", "nvidia", "3572375710", "model1")) != 100*1024*1024 {
		t.Fatalf("Wrong Result in MemoryTotal")
	}

}
