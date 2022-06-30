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
	"fmt"
	"strings"

	"github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia/util"
	"github.com/NVIDIA/gpu-monitoring-tools/bindings/go/nvml"
	"github.com/golang/glog"

	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// GPUHealthChecker checks the health of nvidia GPUs. Note that with the current
// device naming pattern in device manager, GPUHealthChecker will not work with
// MIG devices.
type GPUHealthChecker struct {
	devices           map[string]pluginapi.Device
	nvmlDevices       map[string]*nvml.Device
	health            chan pluginapi.Device
	eventSet          nvml.EventSet
	stop              chan bool
	healthCriticalXid map[uint64]bool
}

// NewGPUHealthChecker returns a GPUHealthChecker object for a given device name
func NewGPUHealthChecker(devices map[string]pluginapi.Device, health chan pluginapi.Device, codes []int) *GPUHealthChecker {
	hc := &GPUHealthChecker{
		devices:           make(map[string]pluginapi.Device),
		nvmlDevices:       make(map[string]*nvml.Device),
		health:            health,
		stop:              make(chan bool),
		healthCriticalXid: make(map[uint64]bool),
	}

	// Cloning the device map to avoid interfering with the device manager
	for id, d := range devices {
		hc.devices[id] = d
	}
	for _, c := range codes {
		glog.Infof("reading code %v", c)
		hc.healthCriticalXid[uint64(c)] = true
	}
	// By default, we check Double Bit ECC Error
	hc.healthCriticalXid[48] = true
	return hc
}

// Start registers NVML events and starts listening to them
func (hc *GPUHealthChecker) Start() error {
	glog.Info("Starting GPU Health Checker")

	for name, device := range hc.devices {
		glog.Infof("Healthchecker receives device %s, device %v+", name, device)
	}

	// Building mapping between device ID and their nvml represetation
	count, err := nvml.GetDeviceCount()
	if err != nil {
		return fmt.Errorf("failed to get device count: %s", err)
	}

	glog.Infof("Found %d GPU devices", count)
	for i := uint(0); i < count; i++ {
		device, err := nvml.NewDeviceLite(i)
		if err != nil {
			return fmt.Errorf("failed to read device with index %d: %v", i, err)
		}

		deviceName, err := util.DeviceNameFromPath(device.Path)
		if err != nil {
			glog.Errorf("Invalid GPU device path found: %s. Skipping this device", device.Path)
			continue
		}

		migEnabled, err := device.IsMigEnabled()
		if err != nil {
			glog.Errorf("Error checking if MIG is enabled on device %s. Skipping this device. Error: %v", deviceName, err)
			continue
		}

		if migEnabled {
			if err := hc.addMigEnabledDevice(deviceName, device); err != nil {
				glog.Errorf("Failed to add MIG-enabled device %s for health check. Skipping this device. Error: %v", deviceName, err)
				continue
			}
		} else {
			hc.addDevice(deviceName, device)
		}
	}

	hc.eventSet = nvml.NewEventSet()
	for _, d := range hc.nvmlDevices {
		gpu, _, _, err := nvml.ParseMigDeviceUUID(d.UUID)
		if err != nil {
			gpu = d.UUID
		}

		glog.Infof("Registering device %v. UUID: %s", d.Path, d.UUID)
		err = nvml.RegisterEventForDevice(hc.eventSet, nvml.XidCriticalError, gpu)
		if err != nil {
			if strings.HasSuffix(err.Error(), "Not Supported") {
				glog.Warningf("Warning: %s is too old to support healthchecking: %v. It will always be marked healthy.", d.Path, err)
				continue
			} else {
				return fmt.Errorf("failed to register device %s for NVML eventSet: %v", d.Path, err)
			}
		}
	}

	go func() {
		if err := hc.listenToEvents(); err != nil {
			glog.Errorf("GPUHealthChecker listenToEvents error: %v", err)
		}
	}()

	return nil
}

func (hc *GPUHealthChecker) addDevice(deviceName string, device *nvml.Device) {
	if _, ok := hc.devices[deviceName]; !ok {
		// Only monitor the devices passed in
		glog.Warningf("Ignoring device %s for health check.", deviceName)
		return
	}
	glog.Infof("Found non-mig device %s for health monitoring. UUID: %s", deviceName, device.UUID)
	hc.nvmlDevices[deviceName] = device
}

func (hc *GPUHealthChecker) addMigEnabledDevice(deviceName string, device *nvml.Device) error {
	glog.Infof("HealthChecker detects MIG is enabled on device %s", deviceName)

	migs, err := device.GetMigDevices()
	if err != nil {
		return fmt.Errorf("error getting MIG devices on device %s. err: %v.", deviceName, err)
	}

	for _, mig := range migs {
		gpu, gi, _, err := nvml.ParseMigDeviceUUID(mig.UUID)
		if err != nil {
			return fmt.Errorf("error parsing MIG UUID on device %s, MIG UUID: %s, error %v", gpu, mig.UUID, err)
		}
		migDeviceName := fmt.Sprintf("%s/gi%d", deviceName, gi)

		if _, ok := hc.devices[migDeviceName]; !ok {
			// Only monitor the devices passed in
			glog.Warningf("Ignoring device %s for health check.", migDeviceName)
			continue
		}
		glog.Infof("Found mig device %s for health monitoring. UUID: %s", migDeviceName, mig.UUID)
		hc.nvmlDevices[migDeviceName] = mig
	}
	return nil
}

type callDevice interface {
	parseMigDeviceUUID(UUID string) (string, uint, uint, error)
}
type GPUDevice struct{}

func (gd *GPUDevice) parseMigDeviceUUID(UUID string) (string, uint, uint, error) {
	return nvml.ParseMigDeviceUUID(UUID)
}

func (hc *GPUHealthChecker) catchError(e nvml.Event, cd callDevice) {
	// Skip the error if it's not Xid critical
	if e.Etype != nvml.XidCriticalError {
		glog.Infof("Skip error Xid=%d as it is not Xid Critical", e.Edata)
		return
	}
	// Only marking device unhealthy on Double Bit ECC Error or customer-configured codes
	// See https://docs.nvidia.com/deploy/xid-errors/index.html#topic_4
	if _, ok := hc.healthCriticalXid[e.Edata]; !ok {
		glog.Infof("Health checker is skipping Xid %v error", e.Edata)
		return
	}

	if e.UUID == nil || len(*e.UUID) == 0 {
		// All devices are unhealthy
		glog.Errorf("XidCriticalError: Xid=%d, All devices will go unhealthy.", e.Edata)
		for id, d := range hc.devices {
			d.Health = pluginapi.Unhealthy
			hc.devices[id] = d
			hc.health <- d
		}
		return
	}

	founderrordevice := false
	for _, d := range hc.devices {
		// Please see https://github.com/NVIDIA/gpu-monitoring-tools/blob/148415f505c96052cb3b7fdf443b34ac853139ec/bindings/go/nvml/nvml.h#L1424
		// for the rationale why gi and ci can be set as such when the UUID is a full GPU UUID and not a MIG device UUID.
		uuid := hc.nvmlDevices[d.ID].UUID
		gpu, gi, ci, err := cd.parseMigDeviceUUID(uuid)
		if err != nil {
			gpu = uuid
			gi = 0xFFFFFFFF
			ci = 0xFFFFFFFF
		}

		if gpu == *e.UUID && gi == *e.GpuInstanceId && ci == *e.ComputeInstanceId {
			glog.Errorf("XidCriticalError: Xid=%d on Device=%s, uuid=%s, the device will go unhealthy.", e.Edata, d.ID, uuid)
			d.Health = pluginapi.Unhealthy
			hc.devices[d.ID] = d
			hc.health <- d
			founderrordevice = true
		}
	}
	if !founderrordevice {
		glog.Errorf("XidCriticalError: Xid=%d on unknown device.", e.Edata)
	}
}

// listenToEvents listens to events from NVML to detect GPU critical errors
func (hc *GPUHealthChecker) listenToEvents() error {
	for {
		select {
		case <-hc.stop:
			close(hc.stop)
			return nil
		default:
		}

		e, err := nvml.WaitForEvent(hc.eventSet, 5000)
		if err != nil {
			continue
		}
		gd := GPUDevice{}
		hc.catchError(e, &gd)
	}
}

// Stop deletes the NVML events and stops the listening go routine
func (hc *GPUHealthChecker) Stop() {
	nvml.DeleteEventSet(hc.eventSet)
	hc.stop <- true
	<-hc.stop
}
