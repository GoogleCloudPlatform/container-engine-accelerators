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
	"regexp"
	"strconv"

	"github.com/golang/glog"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

const nvidiaDeviceRE = `^nvidia[0-9]*$`

// Max number of GPU partitions that can be created for each partition size.
// Source: https://docs.nvidia.com/datacenter/tesla/mig-user-guide/#partitioning
var gpuPartitionSizeMaxCount = map[string]int{
	//nvidia-tesla-a100
	"1g.5gb":  7,
	"2g.10gb": 3,
	"3g.20gb": 2,
	"7g.40gb": 1,
	//nvidia-a100-80gb 
	"1g.10gb": 7,
	"2g.20gb": 3,
	"3g.40gb": 2,
	"7g.80gb": 1,
}

// DeviceManager performs various management operations on mig devices.
type DeviceManager struct {
	devDirectory      string
	procDirectory     string
	gpuPartitionSpecs map[string][]pluginapi.DeviceSpec
	gpuPartitions     map[string]pluginapi.Device
}

// NewDeviceManager creates a new DeviceManager to handle MIG devices on the node.
func NewDeviceManager(devDirectory, procDirectory string) DeviceManager {
	return DeviceManager{
		devDirectory:      devDirectory,
		procDirectory:     procDirectory,
		gpuPartitionSpecs: make(map[string][]pluginapi.DeviceSpec),
		gpuPartitions:     make(map[string]pluginapi.Device),
	}
}

// ListGPUPartitionDevices lists all the GPU partitions as devices that can be advertised as
// resources available on the node.
func (d *DeviceManager) ListGPUPartitionDevices() map[string]pluginapi.Device {
	return d.gpuPartitions
}

// DeviceSpec returns the device spec that inclues list of devices to allocate for a deviceID.
func (d *DeviceManager) DeviceSpec(deviceID string) ([]pluginapi.DeviceSpec, error) {
	deviceSpecs, ok := d.gpuPartitionSpecs[deviceID]
	if !ok {
		return []pluginapi.DeviceSpec{}, fmt.Errorf("invalid allocation request with non-existing GPU partition: %s", deviceID)
	}

	return deviceSpecs, nil
}

// Start method performs the necessary initializations and starts the mig.DeviceManager.
func (d *DeviceManager) Start(partitionSize string) error {
	if partitionSize == "" {
		return nil
	}

	maxPartitionCount, ok := gpuPartitionSizeMaxCount[partitionSize]
	if !ok {
		return fmt.Errorf("%s is not a valid GPU partition size", partitionSize)
	}

	d.gpuPartitionSpecs = make(map[string][]pluginapi.DeviceSpec)

	nvidiaCapDir := path.Join(d.procDirectory, "driver/nvidia/capabilities")
	capFiles, err := ioutil.ReadDir(nvidiaCapDir)
	if err != nil {
		return fmt.Errorf("failed to read capabilities directory (%s): %v", nvidiaCapDir, err)
	}

	gpuFileRegexp := regexp.MustCompile("gpu([0-9]+)")
	giFileRegexp := regexp.MustCompile("gi([0-9]+)")
	deviceRegexp := regexp.MustCompile("DeviceFileMinor: ([0-9]+)")

	numPartitionedGPUs := 0

	for _, capFile := range capFiles {
		m := gpuFileRegexp.FindStringSubmatch(capFile.Name())
		if len(m) != 2 {
			// Not a gpu, continue to next file
			continue
		}

		gpuID := m[1]
		numPartitionedGPUs++

		giBasePath := path.Join(nvidiaCapDir, capFile.Name(), "mig")
		giFiles, err := ioutil.ReadDir(giBasePath)
		if err != nil {
			return fmt.Errorf("failed to read GPU instance capabilities dir (%s): %v", giBasePath, err)
		}

		numPartitions := 0
		for _, giFile := range giFiles {
			if !giFileRegexp.MatchString(giFile.Name()) {
				continue
			}

			numPartitions++

			gpuInstanceID := "nvidia" + gpuID + "/" + giFile.Name()
			giAccessFile := path.Join(giBasePath, giFile.Name(), "access")
			content, err := ioutil.ReadFile(giAccessFile)
			if err != nil {
				return fmt.Errorf("failed to read GPU Instance access file (%s): %v", giAccessFile, err)
			}

			m := deviceRegexp.FindStringSubmatch(string(content))
			if len(m) != 2 {
				return fmt.Errorf("unexpected contents in GPU instance access file(%s): %v", giAccessFile, err)
			}
			giMinorDevice, err := strconv.Atoi(m[1])
			if err != nil {
				return fmt.Errorf("failed to find minor device from GPU instance access file(%s): %v", giAccessFile, err)
			}

			ciAccessFile := path.Join(giBasePath, giFile.Name(), "ci0", "access")
			content, err = ioutil.ReadFile(ciAccessFile)
			if err != nil {
				return fmt.Errorf("unable to read Compute Instance access file (%s): %v", ciAccessFile, err)
			}

			m = deviceRegexp.FindStringSubmatch(string(content))
			if len(m) != 2 {
				return fmt.Errorf("unexpected contents in compute instance access file(%s): %v", ciAccessFile, err)
			}
			ciMinorDevice, err := strconv.Atoi(m[1])
			if err != nil {
				return fmt.Errorf("failed to find minor device from compute instance access file(%s): %v", ciAccessFile, err)
			}

			gpuDevice := path.Join(d.devDirectory, "nvidia"+gpuID)
			if _, err := os.Stat(gpuDevice); err != nil {
				return fmt.Errorf("GPU device (%s) not fount: %v", gpuDevice, err)
			}

			giDevice := path.Join(d.devDirectory, "nvidia-caps", "nvidia-cap"+strconv.Itoa(giMinorDevice))
			if _, err := os.Stat(giDevice); err != nil {
				return fmt.Errorf("GPU instance device (%s) not fount: %v", giDevice, err)
			}

			ciDevice := path.Join(d.devDirectory, "nvidia-caps", "nvidia-cap"+strconv.Itoa(ciMinorDevice))
			if _, err := os.Stat(ciDevice); err != nil {
				return fmt.Errorf("Compute instance device (%s) not fount: %v", ciDevice, err)
			}

			glog.Infof("Discovered GPU partition: %s", gpuInstanceID)
			d.gpuPartitionSpecs[gpuInstanceID] = []pluginapi.DeviceSpec{
				{
					ContainerPath: gpuDevice,
					HostPath:      gpuDevice,
					Permissions:   "mrw",
				},
				{
					ContainerPath: giDevice,
					HostPath:      giDevice,
					Permissions:   "mrw",
				},
				{
					ContainerPath: ciDevice,
					HostPath:      ciDevice,
					Permissions:   "mrw",
				},
			}
			d.gpuPartitions[gpuInstanceID] = pluginapi.Device{ID: gpuInstanceID, Health: pluginapi.Healthy}
		}

		if numPartitions != maxPartitionCount {
			return fmt.Errorf("Number of partitions (%d) for GPU %s does not match expected partition count (%d)", numPartitions, gpuID, maxPartitionCount)
		}
	}

	numGPUs, err := d.discoverNumGPUs()
	if err != nil {
		return err
	}
	if numPartitionedGPUs != numGPUs {
		return fmt.Errorf("Not all GPUs are partitioned as expected. Total number of GPUs: %d, number of partitioned GPUs: %d", numGPUs, numPartitionedGPUs)
	}

	return nil
}

// SetDeviceHealth sets the health status for a GPU partition
func (d *DeviceManager) SetDeviceHealth(name string, health string) {
	d.gpuPartitions[name] = pluginapi.Device{ID: name, Health: health}
}

// Discovers all NVIDIA GPU devices available on the local node by walking nvidiaGPUManager's devDirectory.
func (d *DeviceManager) discoverNumGPUs() (int, error) {
	numGPUs := 0

	reg := regexp.MustCompile(nvidiaDeviceRE)
	files, err := ioutil.ReadDir(d.devDirectory)
	if err != nil {
		return 0, fmt.Errorf("failed to read devices on node: %v", err)
	}

	for _, f := range files {
		if f.IsDir() {
			continue
		}
		if reg.MatchString(f.Name()) {
			numGPUs++
		}
	}
	return numGPUs, nil
}
