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
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/NVIDIA/gpu-monitoring-tools/bindings/go/nvml"
	"github.com/golang/glog"
	"golang.org/x/sys/unix"

	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// TODO: short term, accept nvidia-smi path through commandline.
// Long term, use nvml to handle all MIG operations.
const nvidiaSmiPath = "/usr/local/nvidia/bin/nvidia-smi"

var gpuInstanceToComputeInstanceProfiles = map[int]int{
	nvml.GPU_INSTANCE_PROFILE_1_SLICE: nvml.COMPUTE_INSTANCE_PROFILE_1_SLICE,
	nvml.GPU_INSTANCE_PROFILE_2_SLICE: nvml.COMPUTE_INSTANCE_PROFILE_2_SLICE,
	nvml.GPU_INSTANCE_PROFILE_3_SLICE: nvml.COMPUTE_INSTANCE_PROFILE_3_SLICE,
	nvml.GPU_INSTANCE_PROFILE_4_SLICE: nvml.COMPUTE_INSTANCE_PROFILE_4_SLICE,
	nvml.GPU_INSTANCE_PROFILE_7_SLICE: nvml.COMPUTE_INSTANCE_PROFILE_7_SLICE,
}

// Max number of GPU instances that can be created for each profile.
// Source: https://docs.nvidia.com/datacenter/tesla/mig-user-guide/#partitioning
var gpuInstanceProfileMaxCount = map[int]int{
	nvml.GPU_INSTANCE_PROFILE_1_SLICE: 7,
	nvml.GPU_INSTANCE_PROFILE_2_SLICE: 3,
	nvml.GPU_INSTANCE_PROFILE_3_SLICE: 2,
	nvml.GPU_INSTANCE_PROFILE_4_SLICE: 1,
	nvml.GPU_INSTANCE_PROFILE_7_SLICE: 1,
}

var partitionSizeToProfileMap = map[string]int{
	"1g.5gb":  nvml.GPU_INSTANCE_PROFILE_1_SLICE,
	"2g.10gb": nvml.GPU_INSTANCE_PROFILE_2_SLICE,
	"3g.20gb": nvml.GPU_INSTANCE_PROFILE_3_SLICE,
	"7g.40gb": nvml.GPU_INSTANCE_PROFILE_7_SLICE,
}

// DeviceManager performs various management operations on mig devices.
type DeviceManager struct {
	devDirectory  string
	procDirectory string
	devices       []*nvml.Device // Represents whole GPUs
	gpuPartitions map[string][]pluginapi.DeviceSpec
}

// NewDeviceManager creates a new DeviceManager to handle MIG devices on the node.
func NewDeviceManager(devDirectory, procDirectory string) DeviceManager {
	return DeviceManager{
		devDirectory:  devDirectory,
		procDirectory: procDirectory,
		devices:       make([]*nvml.Device, 0),
		gpuPartitions: make(map[string][]pluginapi.DeviceSpec),
	}
}

// ListGPUPartitionDevices lists all the GPU partitions as devices that can be advertised as
// resources available on the node.
func (d *DeviceManager) ListGPUPartitionDevices() map[string]pluginapi.Device {
	devices := make(map[string]pluginapi.Device)

	for id := range d.gpuPartitions {
		devices[id] = pluginapi.Device{
			ID:     id,
			Health: pluginapi.Healthy,
		}
	}

	return devices
}

// DeviceSpec returns the device spec that inclues list of devices to allocate for a deviceID.
func (d *DeviceManager) DeviceSpec(deviceID string) ([]pluginapi.DeviceSpec, error) {
	deviceSpecs, ok := d.gpuPartitions[deviceID]
	if !ok {
		return []pluginapi.DeviceSpec{}, fmt.Errorf("invalid allocation request with non-existing GPU partition: %s", deviceID)
	}

	return deviceSpecs, nil
}

// Start method performs the necessary initializations and starts the mig.DeviceManager.
func (d *DeviceManager) Start() error {
	if err := nvml.Init(); err != nil {
		return fmt.Errorf("unable to initialize nvml: %v", err)
	}

	gpuCount, err := nvml.GetDeviceCount()
	if err != nil {
		return fmt.Errorf("failed to get device count: %v", err)
	}

	for i := uint(0); i < gpuCount; i++ {
		device, err := nvml.NewDevice(i)
		if err != nil {
			return fmt.Errorf("failed to look up device index: %d, error: %v", i, err)
		}

		d.devices = append(d.devices, device)
	}
	return nil
}

// Stop method performs necessary cleanup operations to safely shut down mig.DeviceManager.
func (d *DeviceManager) Stop() error {
	return nvml.Shutdown()
}

// ConfigureGPUPartitions accepts a string describing the GPU partition type, and partitions all the GPUs on the node as specified.
func (d *DeviceManager) ConfigureGPUPartitions(partition string) error {
	if err := d.enableMIGMode(); err != nil {
		return fmt.Errorf("failed to configure GPU partitions: %v", err)
	}

	if err := deleteAllGPUPartitions(); err != nil {
		return fmt.Errorf("failed to delete existing GPU partitions: %v", err)
	}

	for _, device := range d.devices {
		if err := createPartitionsOnGPU(partition, device); err != nil {
			return fmt.Errorf("failed to create GPU partitions on device: %s, error: %v", device.Path, err)
		}
	}

	if err := d.createDeviceNodes(); err != nil {
		return fmt.Errorf("failed to create device node: %v", err)
	}

	return nil
}

// enableMIGMode ensures that MIG mode is enabled on all GPUs. If all GPUs are already in MIG mode, no action is taken.
// If at least one GPU had to be flipped into MIG mode, the node is restarted as well.
func (d *DeviceManager) enableMIGMode() error {
	rebootNeeded := false

	for _, device := range d.devices {
		migEnabled, err := device.IsMigEnabled()
		if err != nil {
			return fmt.Errorf("failed to check if MIG is enabled on device %s: %v", device.Path, err)
		}
		if migEnabled {
			continue
		}

		// If at least one GPU was not already in MIG mode, we need to reboot the VM.
		rebootNeeded = true

		_, err = device.SetMigMode(nvml.DEVICE_MIG_ENABLE)
		if err != nil {
			return fmt.Errorf("could not enable mig mode on GPU %s: %v", device.Path, err)
		}
	}

	if rebootNeeded {
		glog.Infof("Node needs to rebooted after turning on MIG mode on GPUs")
		err := rebootNode()
		if err != nil {
			return fmt.Errorf("unable to reboot node after enabling mig mode: %v", err)
		}
	}

	return nil
}

func createPartitionsOnGPU(partition string, device *nvml.Device) error {
	giProfile, ok := partitionSizeToProfileMap[partition]
	if !ok {
		return fmt.Errorf("cannot create GPU partition of size %s: invalid partition size", partition)
	}

	for i := 0; i < gpuInstanceProfileMaxCount[giProfile]; i++ {
		profileInfo, err := device.GetGPUInstanceProfileInfo(giProfile)
		if err != nil {
			return fmt.Errorf("failed to get profile info for profile: %d, error: %v", giProfile, err)
		}

		gi, err := device.CreateGPUInstance(&profileInfo)
		if err != nil {
			return fmt.Errorf("failed to create GPU instance on GPU %s, partition size: %s, error: %v", device.Path, partition, err)
		}

		ciProfileInfo, err := gi.GetComputeInstanceProfileInfo(gpuInstanceToComputeInstanceProfiles[giProfile], nvml.COMPUTE_INSTANCE_ENGINE_PROFILE_SHARED)
		if err != nil {
			return fmt.Errorf("failed to get compute instance profile: %v", err)
		}

		_, err = gi.CreateComputeInstance(&ciProfileInfo)
		if err != nil {
			return fmt.Errorf("failed to create compute instance: %v", err)
		}
	}

	return nil
}

func (d *DeviceManager) createDeviceNodes() error {
	majorDeviceRegexp := regexp.MustCompile("([0-9]+) nvidia-caps")
	content, err := ioutil.ReadFile(path.Join(d.procDirectory, "devices"))
	if err != nil {
		return fmt.Errorf("unable to read devices file: %v", err)
	}
	m := majorDeviceRegexp.FindStringSubmatch(string(content))
	if len(m) != 2 {
		return fmt.Errorf("could not find major device number for nvidia-caps device")
	}
	majorDevice, err := strconv.Atoi(m[1])
	if err != nil {
		return fmt.Errorf("major device (%s) is not an integer: %v", m[1], err)
	}

	nvidiaCapDir := path.Join(d.procDirectory, "driver/nvidia/capabilities")
	capFiles, err := ioutil.ReadDir(nvidiaCapDir)
	if err != nil {
		return fmt.Errorf("failed to read capabilities directory: %v", err)
	}

	gpuFileRegexp := regexp.MustCompile("gpu([0-9]+)")
	giFileRegexp := regexp.MustCompile("gi([0-9]+)")
	deviceRegexp := regexp.MustCompile("DeviceFileMinor: ([0-9]+)")

	for _, capFile := range capFiles {
		m := gpuFileRegexp.FindStringSubmatch(capFile.Name())
		if len(m) != 2 {
			// Not a gpu, continue to next file
			continue
		}

		gpuID := m[1]

		giBasePath := path.Join(nvidiaCapDir, capFile.Name(), "mig")
		giFiles, err := ioutil.ReadDir(giBasePath)
		if err != nil {
			return fmt.Errorf("unable to discover gpu instance: %v", err)
		}

		for _, giFile := range giFiles {
			if !giFileRegexp.MatchString(giFile.Name()) {
				continue
			}

			gpuInstanceID := "nvidia" + gpuID + "/" + giFile.Name()
			giAccessFile := path.Join(giBasePath, giFile.Name(), "access")
			content, err := ioutil.ReadFile(giAccessFile)
			if err != nil {
				return fmt.Errorf("unable to read GPU Instance access file (%s): %v", giAccessFile, err)
			}

			m := deviceRegexp.FindStringSubmatch(string(content))
			if len(m) != 2 {
				return fmt.Errorf("unexpected contents in GPU instance access file(%s): %v", giAccessFile, err)
			}
			giMinorDevice, _ := strconv.Atoi(m[1])

			ciAccessFile := path.Join(giBasePath, giFile.Name(), "ci0", "access")
			content, err = ioutil.ReadFile(ciAccessFile)
			if err != nil {
				return fmt.Errorf("unable to read Compute Instance access file (%s): %v", ciAccessFile, err)
			}

			m = deviceRegexp.FindStringSubmatch(string(content))
			if len(m) != 2 {
				return fmt.Errorf("unexpected contents in compute instance access file(%s): %v", ciAccessFile, err)
			}
			ciMinorDevice, _ := strconv.Atoi(m[1])

			gpuDevice := path.Join(d.devDirectory, "nvidia"+gpuID)
			if _, err := os.Stat(gpuDevice); err != nil {
				return fmt.Errorf("GPU device (%s) not fount: %v", gpuDevice, err)
			}

			giDevice := path.Join(d.devDirectory, "nvidia-caps", "nvidia-cap"+strconv.Itoa(giMinorDevice))
			createDeviceNode(giDevice, uint32(majorDevice), uint32(giMinorDevice))
			if _, err := os.Stat(giDevice); err != nil {
				return fmt.Errorf("GPU instance device (%s) not fount: %v", giDevice, err)
			}

			ciDevice := path.Join(d.devDirectory, "nvidia-caps", "nvidia-cap"+strconv.Itoa(ciMinorDevice))
			createDeviceNode(ciDevice, uint32(majorDevice), uint32(ciMinorDevice))
			if _, err := os.Stat(ciDevice); err != nil {
				return fmt.Errorf("Compute instance device (%s) not fount: %v", ciDevice, err)
			}

			d.gpuPartitions[gpuInstanceID] = []pluginapi.DeviceSpec{
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

		}
	}
	return nil
}

func createDeviceNode(path string, major, minor uint32) error {
	if err := syscall.Mknod(path, syscall.S_IFCHR|uint32(os.FileMode(0444)), int(unix.Mkdev(major, minor))); err != nil {
		return fmt.Errorf("Failed to create devive node %s: %v", path, err)
	}
	return nil
}

func deleteAllGPUPartitions() error {
	// TODO: Use NVML to delete GPU partitions.
	// Currently we are unable to use NVML to delete GPU partitions from inside the device plugin container, so using nvidia-smi for now.
	out, err := exec.Command(nvidiaSmiPath, "mig", "-dci").Output()
	if err != nil && !strings.Contains(string(out[:]), "Not Found") {
		return fmt.Errorf("failed to delete compute instances: %v", err)
	}

	out, err = exec.Command(nvidiaSmiPath, "mig", "-dgi").Output()
	if err != nil && !strings.Contains(string(out[:]), "Not Found") {
		return fmt.Errorf("failed to delete GPU instances: %v", err)
	}

	return nil
}

func rebootNode() error {
	return ioutil.WriteFile("/proc/sysrq-trigger", []byte("b"), 0644)
}
