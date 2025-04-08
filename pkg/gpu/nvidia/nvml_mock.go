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

// This file mocks NVML library methods for unit tests.

package nvidia

import (
	"io/ioutil"
	"regexp"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

type mockDeviceInfo struct {
	currentDevice int
	testDevDir    string
	busID         [32]int8
}

func (gpuDeviceInfo *mockDeviceInfo) deviceCount() (int, nvml.Return) {
	reg := regexp.MustCompile(nvidiaDeviceRE)

	files, _ := ioutil.ReadDir(gpuDeviceInfo.testDevDir)

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

func (gpuDeviceInfo *mockDeviceInfo) deviceHandleByIndex(i int) (nvml.Device, nvml.Return) {
	gpuDeviceInfo.currentDevice = i
	return nvml.Device{}, nvml.SUCCESS
}

func (gpuDeviceInfo *mockDeviceInfo) migDeviceHandleByIndex(d nvml.Device, i int) (nvml.Device, nvml.Return) {
	return nvml.Device{}, nvml.SUCCESS
}

func (gpuDeviceInfo *mockDeviceInfo) migMode(d nvml.Device) (int, int, nvml.Return) {
	return 0, 0, nvml.SUCCESS
}

func (gpuDeviceInfo *mockDeviceInfo) minorNumber(d nvml.Device) (int, nvml.Return) {
	return gpuDeviceInfo.currentDevice, nvml.SUCCESS
}

func (gpuDeviceInfo *mockDeviceInfo) pciInfo(d nvml.Device) (nvml.PciInfo, nvml.Return) {
	return nvml.PciInfo{BusId: gpuDeviceInfo.busID}, nvml.SUCCESS
}
