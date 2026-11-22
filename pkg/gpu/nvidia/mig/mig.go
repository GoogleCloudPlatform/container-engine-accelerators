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
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/golang/glog"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

const nvidiaDeviceRE = `^nvidia([0-9]+)$`

var (
	nvidiaDeviceRegexp = regexp.MustCompile(nvidiaDeviceRE)
	gpuFileRegexp      = regexp.MustCompile(`^gpu([0-9]+)$`)
	giFileRegexp       = regexp.MustCompile(`^gi([0-9]+)$`)
	deviceRegexp       = regexp.MustCompile(`DeviceFileMinor: ([0-9]+)`)
)

// Max number of GPU partitions that can be created for each partition size.
// Source: https://docs.nvidia.com/datacenter/tesla/mig-user-guide/#partitioning
var (
	gpuPartitionSizeMaxCount = map[string]int{
		//nvidia-tesla-a100
		"1g.5gb":  7,
		"2g.10gb": 3,
		"3g.20gb": 2,
		"4g.20gb": 1,
		"7g.40gb": 1,
		//nvidia-a100-80gb, nvidia-h100-80gb
		"1g.10gb": 7,
		"2g.20gb": 3,
		"3g.40gb": 2,
		"4g.40gb": 1,
		"7g.80gb": 1,
		//nvidia-h100-80gb
		"1g.20gb": 4,
		//nvidia-h200-141gb
		"1g.18gb":  7,
		"1g.35gb":  4,
		"2g.35gb":  3,
		"3g.71gb":  2,
		"4g.71gb":  1,
		"7g.141gb": 1,
		//nvidia-b200, nvidia-gb200
		"1g.23gb": 7,
		//nvidia-b200
		"1g.45gb":  4,
		"2g.45gb":  3,
		"3g.90gb":  2,
		"4g.90gb":  1,
		"7g.180gb": 1,
		//nvidia-gb200
		"1g.47gb":  4,
		"2g.47gb":  3,
		"3g.93gb":  2,
		"4g.93gb":  1,
		"7g.186gb": 1,
		//nvidia-rtx-pro-6000
		"1g.24gb":        4,
		"1g.24gb+me":     1,
		"1g.24gb+gfx":    4,
		"1g.24gb+me.all": 1,
		"1g.24gb-me":     4,
		"2g.48gb":        2,
		"2g.48gb+gfx":    2,
		"2g.48gb+me.all": 1,
		"2g.48gb-me":     2,
		"4g.96gb":        1,
		"4g.96gb+gfx":    1,
	}
	pciDevicesRoot = "/sys/bus/pci/devices"
)

// DeviceManager performs various management operations on mig devices.
type DeviceManager struct {
	mu                sync.RWMutex
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
	d.mu.RLock()
	defer d.mu.RUnlock()

	out := make(map[string]pluginapi.Device, len(d.gpuPartitions))
	for k, v := range d.gpuPartitions {
		out[k] = v
	}
	return out
}

// PartitionIDs returns the sorted MIG partition device IDs (e.g. "nvidia0/gi1",
// "nvidia0/gi2") currently registered for the given physical GPU ("nvidia0").
func (d *DeviceManager) PartitionIDs(gpu string) []string {
	d.mu.RLock()
	defer d.mu.RUnlock()

	prefix := gpu + "/"
	var ids []string
	for id := range d.gpuPartitions {
		if strings.HasPrefix(id, prefix) {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		giI, okI := parseGIIndex(strings.TrimPrefix(ids[i], prefix))
		giJ, okJ := parseGIIndex(strings.TrimPrefix(ids[j], prefix))
		if okI && okJ && giI != giJ {
			return giI < giJ
		}
		return ids[i] < ids[j]
	})
	return ids
}

func parseGIIndex(giName string) (int, bool) {
	m := giFileRegexp.FindStringSubmatch(giName)
	if len(m) != 2 {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// DeviceSpec returns the device spec that includes list of devices to allocate for a deviceID.
func (d *DeviceManager) DeviceSpec(deviceID string) ([]pluginapi.DeviceSpec, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	deviceSpecs, ok := d.gpuPartitionSpecs[deviceID]
	if !ok {
		return []pluginapi.DeviceSpec{}, fmt.Errorf("invalid allocation request with non-existing GPU partition: %s", deviceID)
	}

	out := make([]pluginapi.DeviceSpec, len(deviceSpecs))
	copy(out, deviceSpecs)
	return out, nil
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

	gpuIDs, err := d.discoverGPUIDs()
	if err != nil {
		return err
	}
	if len(gpuIDs) == 0 {
		return fmt.Errorf("no NVIDIA GPU devices found in %s", d.devDirectory)
	}

	numPartitionsPerGPU, err := d.DiscoverAndMountMIGDevices()
	if err != nil {
		return err
	}

	for gpuID, numPartitions := range numPartitionsPerGPU {
		if numPartitions != maxPartitionCount {
			return fmt.Errorf("number of partitions (%d) for GPU %s does not match expected partition count (%d)", numPartitions, gpuID, maxPartitionCount)
		}
	}

	for _, gpuID := range gpuIDs {
		numPartitions, ok := numPartitionsPerGPU[gpuID]
		if !ok || numPartitions != maxPartitionCount {
			return fmt.Errorf("not all GPUs are partitioned as expected: total number of GPUs: %d, number of partitioned GPUs: %d", len(gpuIDs), len(numPartitionsPerGPU))
		}
	}
	if len(numPartitionsPerGPU) != len(gpuIDs) {
		return fmt.Errorf("not all GPUs are partitioned as expected: total number of GPUs: %d, number of partitioned GPUs: %d", len(gpuIDs), len(numPartitionsPerGPU))
	}

	return nil
}

// DiscoverGPU rediscovers the MIG instances and /dev/nvidia-caps minors for a
// single physical GPU (e.g. "nvidia0") and verifies that its partition count
// matches partitionSize. Other GPUs' partition maps are left untouched.
func (d *DeviceManager) DiscoverGPU(gpu, partitionSize string) error {
	m := nvidiaDeviceRegexp.FindStringSubmatch(gpu)
	if len(m) != 2 {
		return fmt.Errorf("invalid physical GPU name %q", gpu)
	}
	gpuID := m[1]

	maxPartitionCount, ok := gpuPartitionSizeMaxCount[partitionSize]
	if !ok {
		return fmt.Errorf("%s is not a valid GPU partition size", partitionSize)
	}

	specs, partitions, numPartitions, err := d.discoverGPUMIGDevices(gpuID)
	if err != nil {
		return err
	}
	if numPartitions != maxPartitionCount {
		return fmt.Errorf("number of partitions (%d) for GPU %s does not match expected partition count (%d)", numPartitions, gpu, maxPartitionCount)
	}

	prefix := "nvidia" + gpuID + "/"
	d.mu.Lock()
	defer d.mu.Unlock()
	for id := range d.gpuPartitionSpecs {
		if strings.HasPrefix(id, prefix) {
			delete(d.gpuPartitionSpecs, id)
		}
	}
	for id := range d.gpuPartitions {
		if strings.HasPrefix(id, prefix) {
			delete(d.gpuPartitions, id)
		}
	}
	for id, spec := range specs {
		d.gpuPartitionSpecs[id] = spec
	}
	for id, dev := range partitions {
		d.gpuPartitions[id] = dev
	}
	return nil
}

// DiscoverAndMountMIGDevices discovers all the MIG devices on the node and returns a map of GPU ID to the number of partitions.
func (d *DeviceManager) DiscoverAndMountMIGDevices() (map[string]int, error) {
	nvidiaCapDir := path.Join(d.procDirectory, "driver/nvidia/capabilities")
	capFiles, err := ioutil.ReadDir(nvidiaCapDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read capabilities directory (%s): %v", nvidiaCapDir, err)
	}

	numPartitionsPerGPU := make(map[string]int)
	allSpecs := make(map[string][]pluginapi.DeviceSpec)
	allPartitions := make(map[string]pluginapi.Device)

	for _, capFile := range capFiles {
		m := gpuFileRegexp.FindStringSubmatch(capFile.Name())
		if len(m) != 2 {
			// Not a gpu, continue to next file
			continue
		}

		gpuID := m[1]
		specs, partitions, numPartitions, err := d.discoverGPUMIGDevices(gpuID)
		if err != nil {
			return nil, err
		}
		for id, spec := range specs {
			allSpecs[id] = spec
		}
		for id, dev := range partitions {
			allPartitions[id] = dev
		}
		numPartitionsPerGPU[gpuID] = numPartitions
	}

	d.mu.Lock()
	d.gpuPartitionSpecs = allSpecs
	d.gpuPartitions = allPartitions
	d.mu.Unlock()

	return numPartitionsPerGPU, nil
}

func (d *DeviceManager) discoverGPUMIGDevices(gpuID string) (map[string][]pluginapi.DeviceSpec, map[string]pluginapi.Device, int, error) {
	nvidiaCapDir := path.Join(d.procDirectory, "driver/nvidia/capabilities")
	giBasePath := path.Join(nvidiaCapDir, "gpu"+gpuID, "mig")
	giFiles, err := ioutil.ReadDir(giBasePath)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("failed to read GPU instance capabilities dir (%s): %v", giBasePath, err)
	}

	specs := make(map[string][]pluginapi.DeviceSpec)
	partitions := make(map[string]pluginapi.Device)
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
			return nil, nil, 0, fmt.Errorf("failed to read GPU Instance access file (%s): %v", giAccessFile, err)
		}

		m := deviceRegexp.FindStringSubmatch(string(content))
		if len(m) != 2 {
			return nil, nil, 0, fmt.Errorf("unexpected contents in GPU instance access file(%s)", giAccessFile)
		}
		giMinorDevice, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, nil, 0, fmt.Errorf("failed to find minor device from GPU instance access file(%s): %v", giAccessFile, err)
		}

		ciAccessFile := path.Join(giBasePath, giFile.Name(), "ci0", "access")
		content, err = ioutil.ReadFile(ciAccessFile)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("unable to read Compute Instance access file (%s): %v", ciAccessFile, err)
		}

		m = deviceRegexp.FindStringSubmatch(string(content))
		if len(m) != 2 {
			return nil, nil, 0, fmt.Errorf("unexpected contents in compute instance access file(%s)", ciAccessFile)
		}
		ciMinorDevice, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, nil, 0, fmt.Errorf("failed to find minor device from compute instance access file(%s): %v", ciAccessFile, err)
		}

		gpuDevice := path.Join(d.devDirectory, "nvidia"+gpuID)
		if _, err := os.Stat(gpuDevice); err != nil {
			return nil, nil, 0, fmt.Errorf("gpu device (%s) not found: %v", gpuDevice, err)
		}

		giDevice := path.Join(d.devDirectory, "nvidia-caps", "nvidia-cap"+strconv.Itoa(giMinorDevice))
		if _, err := os.Stat(giDevice); err != nil {
			return nil, nil, 0, fmt.Errorf("gpu instance device (%s) not found: %v", giDevice, err)
		}

		ciDevice := path.Join(d.devDirectory, "nvidia-caps", "nvidia-cap"+strconv.Itoa(ciMinorDevice))
		if _, err := os.Stat(ciDevice); err != nil {
			return nil, nil, 0, fmt.Errorf("compute instance device (%s) not found: %v", ciDevice, err)
		}

		glog.Infof("Discovered GPU partition: %s", gpuInstanceID)
		specs[gpuInstanceID] = []pluginapi.DeviceSpec{
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
		topologyInfo, err := d.topology(gpuID)
		if err != nil {
			glog.Errorf("unable to get topology for device with index %s: %v", gpuID, err)
		}
		partitions[gpuInstanceID] = pluginapi.Device{ID: gpuInstanceID, Health: pluginapi.Healthy, Topology: topologyInfo}
	}

	return specs, partitions, numPartitions, nil
}

// SetDeviceHealth sets the health status for a GPU partition
func (d *DeviceManager) SetDeviceHealth(name string, health string, topology *pluginapi.TopologyInfo) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if topology == nil {
		if existing, ok := d.gpuPartitions[name]; ok {
			topology = existing.Topology
		}
	}
	d.gpuPartitions[name] = pluginapi.Device{ID: name, Health: health, Topology: topology}
}

func (d *DeviceManager) discoverGPUIDs() ([]string, error) {
	files, err := ioutil.ReadDir(d.devDirectory)
	if err != nil {
		return nil, fmt.Errorf("failed to read devices on node: %v", err)
	}

	var gpuIDs []string
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		if m := nvidiaDeviceRegexp.FindStringSubmatch(f.Name()); len(m) == 2 {
			gpuIDs = append(gpuIDs, m[1])
		}
	}
	return gpuIDs, nil
}
