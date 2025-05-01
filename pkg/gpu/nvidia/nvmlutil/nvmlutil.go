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

package nvmlutil

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/golang/glog"

	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

type NvmlOperations interface {
	DeviceCount() (int, nvml.Return)
	DeviceHandleByIndex(int) (nvml.Device, nvml.Return)
	MigDeviceHandleByIndex(nvml.Device, int) (nvml.Device, nvml.Return)
	MigMode(nvml.Device) (int, int, nvml.Return)
	MinorNumber(nvml.Device) (int, nvml.Return)
	PciInfo(d nvml.Device) (nvml.PciInfo, nvml.Return)
}

// Declare an interface variable for NVML operations.
// This allows the interface to be overriden with mock
// implementations in the tests.
var NvmlDeviceInfo NvmlOperations

// DeviceInfo is a struct that implements the nvmlOperations interface.
type DeviceInfo struct{}

func (gpuDeviceInfo *DeviceInfo) DeviceCount() (int, nvml.Return) {
	return nvml.DeviceGetCount()
}

func (gpuDeviceInfo *DeviceInfo) DeviceHandleByIndex(i int) (nvml.Device, nvml.Return) {
	return nvml.DeviceGetHandleByIndex(i)
}

func (gpuDeviceInfo *DeviceInfo) MigDeviceHandleByIndex(d nvml.Device, i int) (nvml.Device, nvml.Return) {
	return d.GetMigDeviceHandleByIndex(i)
}

// migMode call's NVML device's GetMigMode() which returns:
// Current mode: The currently active MIG mode
// Pending mode: The MIG mode that will be applied after the next
// GPU reset or system reboot
// Return: NVML return code indicating success or specific error
func (gpuDeviceInfo *DeviceInfo) MigMode(d nvml.Device) (int, int, nvml.Return) {
	return d.GetMigMode()
}

func (gpuDeviceInfo *DeviceInfo) MinorNumber(d nvml.Device) (int, nvml.Return) {
	return d.GetMinorNumber()
}

func (gpuDeviceInfo *DeviceInfo) PciInfo(d nvml.Device) (nvml.PciInfo, nvml.Return) {
	return d.GetPciInfo()
}

// topology determines the NUMA topology information for a GPU device.
// Returns a TopologyInfo containing the NUMA node ID for the GPU device
// if NUMA is enabled, nil otherwise.
// Example for a GPU device associated with NUMA node 1
//
//	topologyInfo := &pluginapi.TopologyInfo{
//	    Nodes: []*pluginapi.NUMANode{
//	        {
//	            ID: 1,
//	        },
//	    },
//	}
func Topology(d nvml.Device, pciDevicesRoot string) (*pluginapi.TopologyInfo, error) {
	if NvmlDeviceInfo == nil {
		NvmlDeviceInfo = &DeviceInfo{}
	}

	numaEnabled, node, err := numaNode(d, pciDevicesRoot)
	if err != nil {
		return nil, err
	}

	if !numaEnabled {
		return nil, nil
	}

	return &pluginapi.TopologyInfo{
		Nodes: []*pluginapi.NUMANode{
			{
				ID: int64(node),
			},
		},
	}, nil
}

// numaNode retrieves the NUMA node information for a given GPU device.
// It first gets the PCI bus ID from the device, formats it appropriately,
// then reads the NUMA node value from the sysfs filesystem.
func numaNode(d nvml.Device, pciDevicesRoot string) (numaEnabled bool, numaNode int, err error) {
	if NvmlDeviceInfo == nil {
		NvmlDeviceInfo = &DeviceInfo{}
	}
	pciInfo, ret := NvmlDeviceInfo.PciInfo(d)
	if ret != nvml.SUCCESS {
		return false, 0, fmt.Errorf("error getting PCI Bus Info of device: %v", ret)
	}

	var bytesT []byte
	for _, b := range pciInfo.BusId {
		if byte(b) == '\x00' {
			break
		}
		bytesT = append(bytesT, byte(b))
	}

	// Discard leading zeros.
	busID := strings.ToLower(strings.TrimPrefix(string(bytesT), "0000"))

	numaNodeFile := fmt.Sprintf("%s/%s/numa_node", pciDevicesRoot, busID)
	glog.V(3).Infof("Reading NUMA node information from %q", numaNodeFile)
	b, err := os.ReadFile(numaNodeFile)
	if err != nil {
		return false, 0, fmt.Errorf("failed to read NUMA information from %q file: %v", numaNodeFile, err)
	}

	numaNode, err = strconv.Atoi(string(bytes.TrimSpace(b)))
	if err != nil {
		return false, 0, fmt.Errorf("eror parsing value for NUMA node: %v", err)
	}

	if numaNode < 0 {
		return false, 0, nil
	}

	return true, numaNode, nil
}
