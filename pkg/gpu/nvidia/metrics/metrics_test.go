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

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

type metricsInfo struct {
	dutyCycle   uint
	usedMemory  uint64
	totalMemory uint64
	uuid        string
	deviceModel string
}

type mockCollector struct{}

func (t *mockCollector) collectGPUDevice(deviceName string) (*nvml.Device, error) {
	return gpuDevicesMock[deviceName], nil
}

func (t *mockCollector) collectDutyCycle(uuid string, since time.Duration) (uint, error) {
	dutyCycle, ok := dutyCycleMock[uuid]
	if !ok {
		return 0, fmt.Errorf("duty cycle for %s not found", uuid)
	}
	return dutyCycle, nil
}

func (t *mockCollector) collectGpuMetricsInfo(device string, d *nvml.Device) (uint, uint64, uint64, string, string, error) {
	info := metricsInfoMock[device]
	return info.dutyCycle, info.usedMemory, info.totalMemory, info.uuid, info.deviceModel, nil
}

var (
	containerDevicesMock = map[ContainerID][]string{
		{
			namespace: "default",
			pod:       "pod1",
			container: "container1",
		}: {
			"nvidia0",
		},
		{
			namespace: "non-default",
			pod:       "pod2",
			container: "container2",
		}: {
			"nvidia1",
			"nvidia2",
		},
	}

	gpuDevicesMock = map[string]*nvml.Device{
		"nvidia0": {},
		"nvidia1": {},
		"nvidia2": {},
		"nvidia3": {},
	}

	dutyCycleMock = map[string]uint{
		"656547758":  78,
		"850729563":  32,
		"3572375710": 13,
		"8732906554": 1,
	}

	metricsInfoMock = map[string]metricsInfo{
		"nvidia0": {
			dutyCycle:   78,
			usedMemory:  uint64(50),
			totalMemory: uint64(200),
			uuid:        "656547758",
			deviceModel: "model1",
		},
		"nvidia1": {
			dutyCycle:   32,
			usedMemory:  uint64(150),
			totalMemory: uint64(200),
			uuid:        "850729563",
			deviceModel: "model2",
		},
		"nvidia2": {
			dutyCycle:   13,
			usedMemory:  uint64(100),
			totalMemory: uint64(350),
			uuid:        "3572375710",
			deviceModel: "model1",
		},
		"nvidia3": {
			dutyCycle:   1,
			usedMemory:  uint64(375),
			totalMemory: uint64(700),
			uuid:        "8732906554",
			deviceModel: "model1",
		},
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
		DutyCycleNodeGpu.WithLabelValues(
			"nvidia", "656547758", "model1")) != 78 ||
		testutil.ToFloat64(
			DutyCycleNodeGpu.WithLabelValues(
				"nvidia", "850729563", "model2")) != 32 ||
		testutil.ToFloat64(
			DutyCycleNodeGpu.WithLabelValues(
				"nvidia", "3572375710", "model1")) != 13 ||
		testutil.ToFloat64(
			DutyCycleNodeGpu.WithLabelValues(
				"nvidia", "8732906554", "model1")) != 1 {
		t.Fatalf("Wrong Result in DutyCycleNodeGpu")
	}

	if testutil.ToFloat64(
		MemoryTotalNodeGpu.WithLabelValues(
			"nvidia", "656547758", "model1")) != 200*1024*1024 ||
		testutil.ToFloat64(
			MemoryTotalNodeGpu.WithLabelValues(
				"nvidia", "850729563", "model2")) != 200*1024*1024 ||
		testutil.ToFloat64(
			MemoryTotalNodeGpu.WithLabelValues(
				"nvidia", "3572375710", "model1")) != 350*1024*1024 ||
		testutil.ToFloat64(
			MemoryTotalNodeGpu.WithLabelValues(
				"nvidia", "8732906554", "model1")) != 700*1024*1024 {
		t.Fatalf("Wrong Result in MemoryTotalNodeGpu")
	}

	if testutil.ToFloat64(
		MemoryUsedNodeGpu.WithLabelValues(
			"nvidia", "656547758", "model1")) != 50*1024*1024 ||
		testutil.ToFloat64(
			MemoryUsedNodeGpu.WithLabelValues(
				"nvidia", "850729563", "model2")) != 150*1024*1024 ||
		testutil.ToFloat64(
			MemoryUsedNodeGpu.WithLabelValues(
				"nvidia", "3572375710", "model1")) != 100*1024*1024 ||
		testutil.ToFloat64(
			MemoryUsedNodeGpu.WithLabelValues(
				"nvidia", "8732906554", "model1")) != 375*1024*1024 {
		t.Fatalf("Wrong Result in MemoryUsedNodeGpu")
	}
}
