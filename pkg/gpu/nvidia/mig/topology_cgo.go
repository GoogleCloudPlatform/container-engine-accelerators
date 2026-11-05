// Copyright 2026 Google Inc. All Rights Reserved.
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

//go:build cgo

package mig

import (
	"fmt"
	"strconv"

	"github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia/nvmlutil"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

func (d *DeviceManager) topology(gpuMinor string) (*pluginapi.TopologyInfo, error) {
	targetMinor, err := strconv.Atoi(gpuMinor)
	if err != nil {
		return nil, fmt.Errorf("unable to convert gpuMinor %q string to int: %v", gpuMinor, err)
	}

	if nvmlutil.NvmlDeviceInfo == nil {
		nvmlutil.NvmlDeviceInfo = &nvmlutil.DeviceInfo{}
	}
	devicesCount, ret := nvmlutil.NvmlDeviceInfo.DeviceCount()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("failed to get devices count: %v", nvml.ErrorString(ret))
	}
	for i := 0; i < devicesCount; i++ {
		device, ret := nvmlutil.NvmlDeviceInfo.DeviceHandleByIndex(i)
		if ret != nvml.SUCCESS {
			continue
		}
		minor, ret := nvmlutil.NvmlDeviceInfo.MinorNumber(device)
		if ret != nvml.SUCCESS {
			continue
		}
		if minor == targetMinor {
			return nvmlutil.Topology(device, pciDevicesRoot)
		}
	}
	return nil, fmt.Errorf("failed to find mig device handle for gpu minor %d", targetMinor)
}
