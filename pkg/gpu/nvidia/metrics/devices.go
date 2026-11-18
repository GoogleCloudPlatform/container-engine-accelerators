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
	"os"
	"regexp"
	"sort"
	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"

	"github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia/gpusharing"
	"github.com/golang/glog"
	"google.golang.org/grpc"
	podresources "k8s.io/kubelet/pkg/apis/podresources/v1alpha1"
)

const (
	// DefaultPodResourcesSocket is the standard Kubelet PodResources Unix socket path.
	DefaultPodResourcesSocket = "/var/lib/kubelet/pod-resources/kubelet.sock"
)

var (
	socketPath      = DefaultPodResourcesSocket
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

// ListPodGPUDevices returns a map of "namespace/pod" to the sorted, unique
// nvidia.com/gpu device IDs (including MIG and virtual sharing IDs) allocated
// to that pod according to the Kubelet PodResources endpoint at socket.
func ListPodGPUDevices(ctx context.Context, socket string) (map[string][]string, error) {
	if socket == "" {
		socket = DefaultPodResourcesSocket
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := os.Stat(socket); err != nil {
		return nil, fmt.Errorf("podresources socket %s: %w", socket, err)
	}

	callCtx, cancel := context.WithTimeout(ctx, connectionTimeout)
	defer cancel()

	conn, err := grpc.DialContext(
		callCtx,
		socket,
		grpc.WithInsecure(),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", addr)
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("error connecting to kubelet PodResourceLister service at %s: %w", socket, err)
	}
	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			glog.Warningf("Failed to close grpc connection to kubelet PodResourceLister endpoint: %v", closeErr)
		}
	}()

	client := podresources.NewPodResourcesListerClient(conn)
	resp, err := client.List(callCtx, &podresources.ListPodResourcesRequest{})
	if err != nil {
		return nil, fmt.Errorf("error listing pod resources: %w", err)
	}

	podDeviceSet := make(map[string]map[string]struct{})
	for _, pod := range resp.GetPodResources() {
		if pod.GetNamespace() == "" && pod.GetName() == "" {
			continue
		}
		podKey := pod.GetNamespace() + "/" + pod.GetName()
		for _, c := range pod.GetContainers() {
			for _, d := range c.GetDevices() {
				if len(d.GetDeviceIds()) == 0 || d.GetResourceName() != gpuResourceName {
					continue
				}
				set := podDeviceSet[podKey]
				if set == nil {
					set = make(map[string]struct{})
					podDeviceSet[podKey] = set
				}
				for _, id := range d.GetDeviceIds() {
					set[id] = struct{}{}
				}
			}
		}
	}

	result := make(map[string][]string, len(podDeviceSet))
	for podKey, set := range podDeviceSet {
		ids := make([]string, 0, len(set))
		for id := range set {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		result[podKey] = ids
	}
	return result, nil
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
	if err != nil {
		return containerDevices, fmt.Errorf("error connecting to kubelet PodResourceLister service: %v", err)
	}
	defer func() {
		err := conn.Close()
		if err != nil {
			glog.Warningf("Failed to close grpc connection to kubelet PodResourceLister endpoint: %v", err)
		}
	}()

	client := podresources.NewPodResourcesListerClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), connectionTimeout)
	defer cancel()
	resp, err := client.List(ctx, &podresources.ListPodResourcesRequest{})
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
				var devices []string
				for _, deviceID := range d.DeviceIds {
					if gpusharing.IsVirtualDeviceID(deviceID) {
						continue
					}
					devices = append(devices, deviceID)
				}
				containerDevices[container] = append(containerDevices[container], devices...)
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
	count, ret := nvml.DeviceGetCount()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("failed to get device count: %s", nvml.ErrorString(ret))
	}

	glog.Infof("Found %d GPU devices", count)
	gpuDevices = make(map[string]*nvml.Device)
	for i := int(0); i < count; i++ {
		device, ret := nvml.DeviceGetHandleByIndex(i)
		if ret != nvml.SUCCESS {
			return fmt.Errorf("failed to read device with index %d: %v", i, nvml.ErrorString(ret))
		}
		minor, ret := device.GetMinorNumber()
		if ret != nvml.SUCCESS {
			glog.Errorf("Invalid GPU device minor number found. Skipping this device")
		}
		deviceName := fmt.Sprintf("nvidia%d", minor)
		glog.Infof("Found device %s for metrics collection", deviceName)
		gpuDevices[deviceName] = &device
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
