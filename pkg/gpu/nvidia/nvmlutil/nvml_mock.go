// Copyright 2025 Google Inc. All Rights Reserved.
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

// This file mocks NVML library methods for unit tests.

package nvmlutil

import (
	"io/ioutil"
	"regexp"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

const nvidiaDeviceRE = `^nvidia[0-9]*$`

type MockDeviceInfo struct {
	CurrentDevice int
	TestDevDir    string
	BusID         [32]int8
}

func (gpuDeviceInfo *MockDeviceInfo) DeviceCount() (int, nvml.Return) {
	reg := regexp.MustCompile(nvidiaDeviceRE)

	files, _ := ioutil.ReadDir(gpuDeviceInfo.TestDevDir)

	numDevices := 0
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		if reg.MatchString(f.Name()) {
			numDevices += 1
		}
	}
	return numDevices, nvml.SUCCESS
}

func (gpuDeviceInfo *MockDeviceInfo) DeviceHandleByIndex(i int) (nvml.Device, nvml.Return) {
	gpuDeviceInfo.CurrentDevice = i
	return nvml.Device{}, nvml.SUCCESS
}

func (gpuDeviceInfo *MockDeviceInfo) MigDeviceHandleByIndex(d nvml.Device, i int) (nvml.Device, nvml.Return) {
	return nvml.Device{}, nvml.SUCCESS
}

func (gpuDeviceInfo *MockDeviceInfo) MigMode(d nvml.Device) (int, int, nvml.Return) {
	return 0, 0, nvml.SUCCESS
}

func (gpuDeviceInfo *MockDeviceInfo) MinorNumber(d nvml.Device) (int, nvml.Return) {
	return gpuDeviceInfo.CurrentDevice, nvml.SUCCESS
}

func (gpuDeviceInfo *MockDeviceInfo) PciInfo(d nvml.Device) (nvml.PciInfo, nvml.Return) {
	return nvml.PciInfo{BusId: gpuDeviceInfo.BusID}, nvml.SUCCESS
}
