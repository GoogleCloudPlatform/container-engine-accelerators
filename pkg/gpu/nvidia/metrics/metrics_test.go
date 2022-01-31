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

type mockCollector struct{}

func (t *mockCollector) collectGPUDevice(deviceName string) (*nvml.Device, error) {
	return gpuDevicesMock[deviceName], nil
}

func (t *mockCollector) collectStatus(d *nvml.Device) (status *nvml.DeviceStatus, err error) {
	return deviceToStatus[d], nil
}

func (t *mockCollector) collectDutyCycle(uuid string, since time.Duration) (uint, error) {
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

	device1 = &nvml.Device{
		UUID:   "656547758",
		Model:  stringPtr("model1"),
		Memory: uint64Ptr(200),
	}
	device2 = &nvml.Device{
		UUID:   "850729563",
		Model:  stringPtr("model2"),
		Memory: uint64Ptr(200),
	}
	device3 = &nvml.Device{
		UUID:   "3572375710",
		Model:  stringPtr("model1"),
		Memory: uint64Ptr(350),
	}
	device4 = &nvml.Device{
		UUID:   "8732906554",
		Model:  stringPtr("model1"),
		Memory: uint64Ptr(700),
	}

	gpuDevicesMock = map[string]*nvml.Device{
		"q759757": device1,
		"afjodaj": device2,
		"7v89zhi": device3,
		"8g45fc3": device4,
	}
	deviceToStatus = map[*nvml.Device]*nvml.DeviceStatus{
		device1: {
			Memory: nvml.MemoryInfo{
				Global: nvml.DeviceMemory{
					Used: uint64Ptr(50),
				},
			},
		},
		device2: {
			Memory: nvml.MemoryInfo{
				Global: nvml.DeviceMemory{
					Used: uint64Ptr(150),
				},
			},
		},
		device3: {
			Memory: nvml.MemoryInfo{
				Global: nvml.DeviceMemory{
					Used: uint64Ptr(100),
				},
			},
		},
		device4: {
			Memory: nvml.MemoryInfo{
				Global: nvml.DeviceMemory{
					Used: uint64Ptr(375),
				},
			},
		},
	}

	dutyCycleMock = map[string]uint{
		"656547758":  78,
		"850729563":  32,
		"3572375710": 13,
		"8732906554": 1,
	}
)

func TestMetricsUpdate(t *testing.T) {
	gmc = &mockCollector{}
	ms := MetricServer{}
	ms.updateMetrics(containerDevicesMock, gpuDevicesMock)

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

	if testutil.ToFloat64(
		DutyCycleGpu.WithLabelValues(
			"nvidia", "656547758", "model1")) != 78 ||
		testutil.ToFloat64(
			DutyCycleGpu.WithLabelValues(
				"nvidia", "850729563", "model2")) != 32 ||
		testutil.ToFloat64(
			DutyCycleGpu.WithLabelValues(
				"nvidia", "3572375710", "model1")) != 13 ||
		testutil.ToFloat64(
			DutyCycleGpu.WithLabelValues(
				"nvidia", "8732906554", "model1")) != 1 {
		t.Fatalf("Wrong Result in DutyCycleGpu")
	}

	if testutil.ToFloat64(
		MemoryTotalGpu.WithLabelValues(
			"nvidia", "656547758", "model1")) != 200*1024*1024 ||
		testutil.ToFloat64(
			MemoryTotalGpu.WithLabelValues(
				"nvidia", "850729563", "model2")) != 200*1024*1024 ||
		testutil.ToFloat64(
			MemoryTotalGpu.WithLabelValues(
				"nvidia", "3572375710", "model1")) != 350*1024*1024 ||
		testutil.ToFloat64(
			MemoryTotalGpu.WithLabelValues(
				"nvidia", "8732906554", "model1")) != 700*1024*1024 {
		t.Fatalf("Wrong Result in MemoryTotalGpu")
	}

	if testutil.ToFloat64(
		MemoryUsedGpu.WithLabelValues(
			"nvidia", "656547758", "model1")) != 50*1024*1024 ||
		testutil.ToFloat64(
			MemoryUsedGpu.WithLabelValues(
				"nvidia", "850729563", "model2")) != 150*1024*1024 ||
		testutil.ToFloat64(
			MemoryUsedGpu.WithLabelValues(
				"nvidia", "3572375710", "model1")) != 100*1024*1024 ||
		testutil.ToFloat64(
			MemoryUsedGpu.WithLabelValues(
				"nvidia", "8732906554", "model1")) != 375*1024*1024 {
		t.Fatalf("Wrong Result in MemoryUsedGpu")
	}
}
