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

package metrics

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"time"

	"github.com/NVIDIA/gpu-monitoring-tools/bindings/go/nvml"

	"github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia/gpusharing"
	"github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia/util"

	"github.com/golang/glog"
	"google.golang.org/grpc"
	podresources "k8s.io/kubernetes/pkg/kubelet/apis/podresources/v1alpha1"
)

var (
	socketPath      = "/var/lib/kubelet/pod-resources/kubelet.sock"
	gpuResourceName = "nvidia.com/gpu"
	gpuPathRegex    = regexp.MustCompile("/dev/(nvidia[0-9]+)$")

	connectionTimeout = 10 * time.Second

	gpuDevices map[string]*nvml.Device
)

// ContainerID uniquely identifies a container.
type ContainerID struct {
	namespace string
	pod       string
	container string
}

// GetDevicesForAllContainers returns a map with container as the key and the list of devices allocated to that container as the value.
// It will skip time-shared GPU devices when time-sharing solution is enabled.
func GetDevicesForAllContainers() (map[ContainerID][]string, error) {
	containerDevices := make(map[ContainerID][]string)
	conn, err := grpc.Dial(
		socketPath,
		grpc.WithInsecure(),
		grpc.WithDialer(func(addr string, timeout time.Duration) (net.Conn, error) {
			return net.DialTimeout("unix", addr, timeout)
		}))
	defer func() {
		err := conn.Close()
		if err != nil {
			glog.Warningf("Failed to close grpc connection to kubelet PodResourceLister endpoint: %v", err)
		}
	}()

	if err != nil {
		return containerDevices, fmt.Errorf("error connecting to kubelet PodResourceLister service: %v", err)
	}
	client := podresources.NewPodResourcesListerClient(conn)

	resp, err := client.List(context.Background(), &podresources.ListPodResourcesRequest{})
	if err != nil {
		return containerDevices, fmt.Errorf("error listing pod resources: %v", err)
	}

	for _, pod := range resp.PodResources {
		container := ContainerID{
			namespace: pod.Namespace,
			pod:       pod.Name,
		}

		for _, c := range pod.Containers {
			container.container = c.Name
			for _, d := range c.Devices {
				if len(d.DeviceIds) == 0 || d.ResourceName != gpuResourceName {
					continue
				}
				containerDevices[container] = make([]string, 0)
				for _, deviceID := range d.DeviceIds {
					if gpusharing.IsVirtualDeviceID(deviceID) {
						continue
					}
					containerDevices[container] = append(containerDevices[container], deviceID)
				}
			}
		}
	}

	return containerDevices, nil
}

func GetAllGpuDevices() map[string]*nvml.Device {
	return gpuDevices
}

// DiscoverGPUDevices discovers GPUs attached to the node, and updates `gpuDevices` map.
func DiscoverGPUDevices() error {
	count, err := nvml.GetDeviceCount()
	if err != nil {
		return fmt.Errorf("failed to get device count: %s", err)
	}

	glog.Infof("Foud %d GPU devices", count)
	gpuDevices = make(map[string]*nvml.Device)
	for i := uint(0); i < count; i++ {
		device, err := nvml.NewDevice(i)
		if err != nil {
			return fmt.Errorf("failed to read device with index %d: %v", i, err)
		}
		deviceName, err := util.DeviceNameFromPath(device.Path)
		if err != nil {
			glog.Errorf("Invalid GPU device path found: %s. Skipping this device", device.Path)
		}
		glog.Infof("Found device %s for metrics collection", deviceName)
		gpuDevices[deviceName] = device
	}

	return nil
}

// DeviceFromName returns the device object for a given device name.
func DeviceFromName(deviceName string) (*nvml.Device, error) {
	device, ok := gpuDevices[deviceName]
	if !ok {
		return &nvml.Device{}, fmt.Errorf("device %s not found", deviceName)
	}

	return device, nil
}
