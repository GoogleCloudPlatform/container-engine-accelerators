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

package healthcheck

import (
	"testing"

	"github.com/NVIDIA/gpu-monitoring-tools/bindings/go/nvml"
	"k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"

	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

func pointer[T any](s T) *T {
	return &s
}

type mockGPUDevice struct{}

func (gp *mockGPUDevice) parseMigDeviceUUID(UUID string) (string, uint, uint, error) {
	return UUID, 3173334309191009974, 1015241, nil
}

func TestCatchError(t *testing.T) {
	gp := mockGPUDevice{}
	device1 := v1beta1.Device{
		ID: "device1",
	}
	udevice1 := v1beta1.Device{
		ID:     "device1",
		Health: pluginapi.Unhealthy,
	}
	device2 := v1beta1.Device{
		ID: "device2",
	}
	udevice2 := v1beta1.Device{
		ID:     "device2",
		Health: pluginapi.Unhealthy,
	}
	tests := []struct {
		name             string
		event            nvml.Event
		hc               GPUHealthChecker
		wantErrorDevices []v1beta1.Device
	}{
		{
			name: "non-critical error",
			event: nvml.Event{
				UUID:              pointer("GPU-f053fce6-851c-1235-90ae-037069703604"),
				GpuInstanceId:     pointer(uint(3173334309191009974)),
				ComputeInstanceId: pointer(uint(1015241)),
				Etype:             0,
				Edata:             uint64(72),
			},
			hc: GPUHealthChecker{
				devices: map[string]v1beta1.Device{
					"device1": device1,
					"device2": device2,
				},
				nvmlDevices: map[string]*nvml.Device{
					"device1": {
						UUID: "GPU-f053fce6-851c-1235-90ae-037069703604",
					},
					"device2": {
						UUID: "GPU-f053fce6-851c-1235-90ae-037069703633",
					},
				},
				healthCriticalXid: map[uint64]bool{
					72: true,
					48: true,
				},
			},
			wantErrorDevices: []v1beta1.Device{},
		},
		{
			name: "xid error not included ",
			event: nvml.Event{
				UUID:              pointer("GPU-f053fce6-851c-1235-90ae-037069703604"),
				GpuInstanceId:     pointer(uint(3173334309191009974)),
				ComputeInstanceId: pointer(uint(1015241)),
				Etype:             nvml.XidCriticalError,
				Edata:             uint64(88),
			},
			hc: GPUHealthChecker{
				devices: map[string]v1beta1.Device{
					"device1": device1,
					"device2": device2,
				},
				nvmlDevices: map[string]*nvml.Device{
					"device1": {
						UUID: "GPU-f053fce6-851c-1235-90ae-037069703604",
					},
					"device2": {
						UUID: "GPU-f053fce6-851c-1235-90ae-037069703633",
					},
				},
				healthCriticalXid: map[uint64]bool{
					72: true,
					48: true,
				},
			},
			wantErrorDevices: []v1beta1.Device{},
		},
		{
			name: "catching xid 72",
			event: nvml.Event{
				UUID:              pointer("GPU-f053fce6-851c-1235-90ae-037069703604"),
				GpuInstanceId:     pointer(uint(3173334309191009974)),
				ComputeInstanceId: pointer(uint(1015241)),
				Etype:             nvml.XidCriticalError,
				Edata:             uint64(72),
			},
			hc: GPUHealthChecker{
				devices: map[string]v1beta1.Device{
					"device1": device1,
					"device2": device2,
				},
				nvmlDevices: map[string]*nvml.Device{
					"device1": {
						UUID: "GPU-f053fce6-851c-1235-90ae-037069703604",
					},
					"device2": {
						UUID: "GPU-f053fce6-851c-1235-90ae-037069703633",
					},
				},
				healthCriticalXid: map[uint64]bool{
					72: true,
					48: true,
				},
			},
			wantErrorDevices: []v1beta1.Device{udevice1},
		},
		{
			name: "unknown device",
			event: nvml.Event{
				UUID:              pointer("GPU-f053fce6-90ae-037069703604"),
				GpuInstanceId:     pointer(uint(3173334309191009974)),
				ComputeInstanceId: pointer(uint(1015241)),
				Etype:             nvml.XidCriticalError,
				Edata:             uint64(72),
			},
			hc: GPUHealthChecker{
				devices: map[string]v1beta1.Device{
					"device1": device1,
					"device2": device2,
				},
				nvmlDevices: map[string]*nvml.Device{
					"device1": {
						UUID: "GPU-f053fce6-851c-1235-90ae-037069703604",
					},
					"device2": {
						UUID: "GPU-f053fce6-851c-1235-90ae-037069703633",
					},
				},
				healthCriticalXid: map[uint64]bool{
					72: true,
					48: true,
				},
			},
			wantErrorDevices: []v1beta1.Device{},
		},
		{
			name: "not catching xid 72",
			event: nvml.Event{
				UUID:              pointer("GPU-f053fce6-851c-1235-90ae-037069703604"),
				GpuInstanceId:     pointer(uint(3173334309191009974)),
				ComputeInstanceId: pointer(uint(1015241)),
				Etype:             nvml.XidCriticalError,
				Edata:             uint64(72),
			},
			hc: GPUHealthChecker{
				devices: map[string]v1beta1.Device{
					"device1": device1,
				},
				nvmlDevices: map[string]*nvml.Device{
					"device1": {
						UUID: "GPU-f053fce6-851c-1235-90ae-037069703604",
					},
				},
				healthCriticalXid: map[uint64]bool{},
			},
			wantErrorDevices: []v1beta1.Device{},
		},
		{
			name: "catching all devices error",
			event: nvml.Event{
				UUID:              nil,
				GpuInstanceId:     pointer(uint(3173334309191009974)),
				ComputeInstanceId: pointer(uint(1015241)),
				Etype:             nvml.XidCriticalError,
				Edata:             uint64(48),
			},
			hc: GPUHealthChecker{
				devices: map[string]v1beta1.Device{
					"device1": device1,
					"device2": device2,
				},
				nvmlDevices: map[string]*nvml.Device{
					"device1": {
						UUID: "GPU-f053fce6-851c-1235-90ae-037069703604",
					},
					"device2": {
						UUID: "GPU-f053fce6-851c-1235-90ae-037069703633",
					},
				},
				healthCriticalXid: map[uint64]bool{
					72: true,
					48: true,
				},
			},
			wantErrorDevices: []v1beta1.Device{udevice1, udevice2},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.hc.health = make(chan v1beta1.Device, len(tt.hc.devices))
			tt.hc.catchError(tt.event, &gp)
			for _, d := range tt.wantErrorDevices {
				if len(tt.hc.health) == 0 {
					t.Errorf("Fewer error devices was caught than expected.")
				} else if gotErrorDevice := <-tt.hc.health; gotErrorDevice != d {
					t.Errorf("Error device was not caught. Got %v. Want %v",
						gotErrorDevice, d)
				}
			}
			if len(tt.hc.health) != 0 {
				t.Errorf("More error devices was caught than expected.")
			}
		})
	}
}
