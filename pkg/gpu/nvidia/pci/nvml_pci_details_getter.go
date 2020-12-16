// Copyright 2020 Google Inc. All Rights Reserved.
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

package pci

import (
	"fmt"
	"github.com/NVIDIA/gpu-monitoring-tools/bindings/go/nvml"
	"github.com/golang/glog"
	"strings"
	"sync"
)

// NewNvmlPciDetailsGetter returns a PciDetailsGetter that uses Nvidia's NVML library to map device id to PCI bus id.
func NewNvmlPciDetailsGetter() (PciDetailsGetter, error) {
	return &nvmlPciDetailsGetter{deviceIDToBusID: nil}, nil
}

func (dg *nvmlPciDetailsGetter) init() {
	numDevices, err := nvml.GetDeviceCount()
	if err != nil {
		glog.Errorf("Failed to get device count: %v", err)
		return
	}
	glog.Infof("Found %d GPUs", numDevices)

	deviceIDToBusID := make(map[string]string)
	for deviceIndex := uint(0); deviceIndex < numDevices; deviceIndex++ {
		device, err := nvml.NewDevice(deviceIndex)
		if err != nil {
			glog.Errorf("Failed to read device with index %d: %v", deviceIndex, err)
			return
		}
		deviceID := strings.Replace(device.Path, "/dev/", "", 1)
		pciBusID := device.PCI.BusID
		glog.Infof("Mapped GPU %s to PCI bus id %s", deviceID, pciBusID)
		deviceIDToBusID[deviceID] = pciBusID
	}
	dg.deviceIDToBusID = deviceIDToBusID
}

type nvmlPciDetailsGetter struct {
	deviceIDToBusID map[string]string
	initOnce        sync.Once
}

func (dg *nvmlPciDetailsGetter) GetPciBusID(deviceID string) (string, error) {
	dg.initOnce.Do(func() { dg.init() })

	if dg.deviceIDToBusID == nil {
		return "", fmt.Errorf("Init of nvmlPciDetailsGetter has failed")
	}

	busID, exists := dg.deviceIDToBusID[deviceID]
	if !exists {
		return "", fmt.Errorf("Could not find GPU \"%s\"", deviceID)
	}
	return busID, nil
}
