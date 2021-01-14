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
	devices     map[string]pluginapi.Device
	nvmlDevices map[string]*nvml.Device
	health      chan pluginapi.Device
	eventSet    nvml.EventSet
	stop        chan bool
}

// NewGPUHealthChecker returns a GPUHealthChecker object for a given device name
func NewGPUHealthChecker(devices map[string]pluginapi.Device, health chan pluginapi.Device) *GPUHealthChecker {
	return &GPUHealthChecker{
		devices:     devices,
		nvmlDevices: make(map[string]*nvml.Device),
		health:      health,
		stop:        make(chan bool),
	}
}

// Start registers NVML events and starts listening to them
func (hc *GPUHealthChecker) Start() error {
	glog.Info("Starting GPU Health Checker")

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

		if _, ok := hc.devices[deviceName]; !ok {
			// we only monitor the devices passed in
			glog.Warningf("Ignoring device %s for health check.", deviceName)
			continue
		}

		glog.Infof("Found device %s for health monitoring. UUID: %s", deviceName, device.UUID)
		hc.nvmlDevices[deviceName] = device
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
		if err != nil || e.Etype != nvml.XidCriticalError {
			glog.Infof("XidCriticalError: Xid=%d, All devices will go unhealthy.", e.Edata)
			continue
		}

		// Ignoring application errors. GPU should still be healthy
		// See https://docs.nvidia.com/deploy/xid-errors/index.html#topic_4
		if e.Edata == 31 || e.Edata == 43 || e.Edata == 45 {
			continue
		}

		if e.UUID == nil || len(*e.UUID) == 0 {
			// All devices are unhealthy
			glog.Errorf("XidCriticalError: Xid=%d, All devices will go unhealthy.", e.Edata)
			for id, d := range hc.devices {
				d.Health = pluginapi.Unhealthy
				hc.devices[id] = d
				hc.health <- d
			}
			continue
		}

		for _, d := range hc.devices {
			// Please see https://github.com/NVIDIA/gpu-monitoring-tools/blob/148415f505c96052cb3b7fdf443b34ac853139ec/bindings/go/nvml/nvml.h#L1424
			// for the rationale why gi and ci can be set as such when the UUID is a full GPU UUID and not a MIG device UUID.
			uuid := hc.nvmlDevices[d.ID].UUID
			gpu, gi, ci, err := nvml.ParseMigDeviceUUID(uuid)
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
			}
		}
	}
}

// Stop deletes the NVML events and stops the listening go routine
func (hc *GPUHealthChecker) Stop() {
	nvml.DeleteEventSet(hc.eventSet)
	hc.stop <- true
	<-hc.stop
}
