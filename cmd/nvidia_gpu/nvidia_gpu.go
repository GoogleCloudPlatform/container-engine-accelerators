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

package main

import (
	"flag"
	"fmt"
	"time"

	gpumanager "github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia"
	"github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia/metrics"
	"github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia/numa"
	"github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia/pci"

	"github.com/NVIDIA/gpu-monitoring-tools/bindings/go/nvml"
	"github.com/golang/glog"
)

const (
	// Device plugin settings.
	kubeletEndpoint      = "kubelet.sock"
	pluginEndpointPrefix = "nvidiaGPU"
	devDirectory         = "/dev"
)

var (
	hostPathPrefix                 = flag.String("host-path", "/home/kubernetes/bin/nvidia", "Path on the host that contains nvidia libraries. This will be mounted inside the container as '-container-path'")
	containerPathPrefix            = flag.String("container-path", "/usr/local/nvidia", "Path on the container that mounts '-host-path'")
	hostVulkanICDPathPrefix        = flag.String("host-vulkan-icd-path", "/home/kubernetes/bin/nvidia/vulkan/icd.d", "Path on the host that contains the Nvidia Vulkan installable client driver. This will be mounted inside the container as '-container-vulkan-icd-path'")
	containerVulkanICDPathPrefix   = flag.String("container-vulkan-icd-path", "/etc/vulkan/icd.d", "Path on the container that mounts '-host-vulkan-icd-path'")
	pluginMountPath                = flag.String("plugin-directory", "/device-plugin", "The directory path to create plugin socket")
	enableContainerGPUMetrics      = flag.Bool("enable-container-gpu-metrics", false, "If true, the device plugin will expose GPU metrics for containers with allocated GPU")
	gpuMetricsPort                 = flag.Int("gpu-metrics-port", 2112, "Port on which GPU metrics for containers are exposed")
	gpuMetricsCollectionIntervalMs = flag.Int("gpu-metrics-collection-interval", 30000, "Colection interval (in milli seconds) for container GPU metrics")
	topologyEnabled                = flag.Bool("topology", false, "Report NUMA node info for use by Kubernetes TopologyManager")
)

func main() {
	flag.Parse()
	glog.Infoln("device-plugin started")
	mountPaths := []gpumanager.MountPath{
		{HostPath: *hostPathPrefix, ContainerPath: *containerPathPrefix},
		{HostPath: *hostVulkanICDPathPrefix, ContainerPath: *containerVulkanICDPathPrefix}}

	if *topologyEnabled || *enableContainerGPUMetrics {
		err := nvml.Init()
		if err != nil {
			glog.Errorf("Failed to initialize NVML: %v", err)
			return
		}
		defer nvml.Shutdown()

		driverVersion, err := nvml.GetDriverVersion()
		if err != nil {
			glog.Errorf("Failed to get NVML driver version: %v", err)
			return
		}
		glog.Infof("NVML initialized successfully. Driver version: %s", driverVersion)
	}

	numaNodeGetter := numa.NewNullNumaNodeGetter()
	if *topologyEnabled {
		pciDetailsGetter, err := pci.NewNvmlPciDetailsGetter()
		if err == nil {
			numaNodeGetter = numa.NewSysNumaNodeGetter("/sys", pciDetailsGetter)
		} else {
			glog.Errorf("NewNvmlPciDetailsGetter failed: %v", err)
		}
	}

	ngm := gpumanager.NewNvidiaGPUManager(devDirectory, mountPaths, numaNodeGetter)
	// Keep on trying until success. This is required
	// because Nvidia drivers may not be installed initially.
	for {
		err := ngm.Start()
		if err == nil {
			break
		}
		// Use non-default level to avoid log spam.
		glog.V(3).Infof("nvidiaGPUManager.Start() failed: %v", err)
		time.Sleep(5 * time.Second)
	}

	if *enableContainerGPUMetrics {
		glog.Infof("Starting metrics server on port: %d, endpoint path: %s, collection frequency: %d", *gpuMetricsPort, "/metrics", *gpuMetricsCollectionIntervalMs)
		metricServer := metrics.NewMetricServer(*gpuMetricsCollectionIntervalMs, *gpuMetricsPort, "/metrics")
		err := metricServer.Start()
		if err != nil {
			glog.Infof("Failed to start metric server: %v", err)
			return
		}
	}

	ngm.Serve(*pluginMountPath, kubeletEndpoint, fmt.Sprintf("%s-%d.sock", pluginEndpointPrefix, time.Now().Unix()))
}
