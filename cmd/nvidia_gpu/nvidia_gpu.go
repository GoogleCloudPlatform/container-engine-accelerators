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
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"time"

	gpumanager "github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia"
	healthcheck "github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia/health_check"
	"github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia/metrics"
	"github.com/NVIDIA/gpu-monitoring-tools/bindings/go/nvml"
	"github.com/golang/glog"
)

const (
	// Device plugin settings.
	kubeletEndpoint      = "kubelet.sock"
	pluginEndpointPrefix = "nvidiaGPU"
	devDirectory         = "/dev"
	// Proc directory is used to lookup the access files for each GPU partition.
	procDirectory = "/proc"
)

var (
	hostPathPrefix                 = flag.String("host-path", "/home/kubernetes/bin/nvidia", "Path on the host that contains nvidia libraries. This will be mounted inside the container as '-container-path'")
	containerPathPrefix            = flag.String("container-path", "/usr/local/nvidia", "Path on the container that mounts '-host-path'")
	hostVulkanICDPathPrefix        = flag.String("host-vulkan-icd-path", "/home/kubernetes/bin/nvidia/vulkan/icd.d", "Path on the host that contains the Nvidia Vulkan installable client driver. This will be mounted inside the container as '-container-vulkan-icd-path'")
	containerVulkanICDPathPrefix   = flag.String("container-vulkan-icd-path", "/etc/vulkan/icd.d", "Path on the container that mounts '-host-vulkan-icd-path'")
	pluginMountPath                = flag.String("plugin-directory", "/device-plugin", "The directory path to create plugin socket")
	enableContainerGPUMetrics      = flag.Bool("enable-container-gpu-metrics", false, "If true, the device plugin will expose GPU metrics for containers with allocated GPU")
	enableHealthMonitoring         = flag.Bool("enable-health-monitoring", false, "If true, the device plugin will detect critical Xid errors and mark the GPUs unallocatable")
	gpuMetricsPort                 = flag.Int("gpu-metrics-port", 2112, "POrt on which GPU metrics for containers are exposed")
	gpuMetricsCollectionIntervalMs = flag.Int("gpu-metrics-collection-interval", 30000, "Colection interval (in milli seconds) for container GPU metrics")
	gpuConfigFile                  = flag.String("gpu-config", "/etc/nvidia/gpu_config.json", "File with GPU configurations for device plugin")
)

func parseGPUConfig(gpuConfigFile string) (gpumanager.GPUConfig, error) {
	var gpuConfig gpumanager.GPUConfig

	gpuConfigContent, err := ioutil.ReadFile(gpuConfigFile)
	if err != nil {
		return gpuConfig, fmt.Errorf("unable to read gpu config file %s: %v", gpuConfigFile, err)
	}

	if err = json.Unmarshal(gpuConfigContent, &gpuConfig); err != nil {
		return gpuConfig, fmt.Errorf("failed to parse GPU config file contents: %s, error: %v", gpuConfigContent, err)
	}
	return gpuConfig, nil
}

func main() {
	flag.Parse()
	glog.Infoln("device-plugin started")
	mountPaths := []gpumanager.MountPath{
		{HostPath: *hostPathPrefix, ContainerPath: *containerPathPrefix},
		{HostPath: *hostVulkanICDPathPrefix, ContainerPath: *containerVulkanICDPathPrefix}}

	var gpuConfig gpumanager.GPUConfig
	if *gpuConfigFile != "" {
		glog.Infof("Reading GPU config file: %s", *gpuConfigFile)
		var err error
		gpuConfig, err = parseGPUConfig(*gpuConfigFile)
		if err != nil {
			glog.Infof("Failed to parse GPU config file %s: %v", *gpuConfigFile, err)
			glog.Infof("Falling back to default GPU config.")
			gpuConfig = gpumanager.GPUConfig{}
		}
	}
	glog.Infof("Using gpu config: %v", gpuConfig)
	ngm := gpumanager.NewNvidiaGPUManager(devDirectory, procDirectory, mountPaths, gpuConfig)

	// Retry until nvidiactl and nvidia-uvm are detected. This is required
	// because Nvidia drivers may not be installed initially.
	for {
		err := ngm.CheckDevicePaths()
		if err == nil {
			if err = ngm.Start(); err == nil {
				break
			}
		}
		// Use non-default level to avoid log spam.
		glog.V(3).Infof("nvidiaGPUManager.CheckDevicePaths() failed: %v", err)
		time.Sleep(5 * time.Second)
	}

	if err := nvml.Init(); err != nil {
		glog.Errorf("failed to initialize nvml: %v", err)
		return
	}
	defer nvml.Shutdown()

	if *enableContainerGPUMetrics {
		glog.Infof("Starting metrics server on port: %d, endpoint path: %s, collection frequency: %d", *gpuMetricsPort, "/metrics", *gpuMetricsCollectionIntervalMs)
		metricServer := metrics.NewMetricServer(*gpuMetricsCollectionIntervalMs, *gpuMetricsPort, "/metrics")
		err := metricServer.Start()
		if err != nil {
			glog.Infof("Failed to start metric server: %v", err)
			return
		}
		defer metricServer.Stop()
	}

	if *enableHealthMonitoring {
		hc := healthcheck.NewGPUHealthChecker(ngm.ListPhysicalDevices(), ngm.Health)
		if err := hc.Start(); err != nil {
			glog.Infof("Failed to start GPU Health Checker: %v", err)
			return
		}
		defer hc.Stop()
	}

	ngm.Serve(*pluginMountPath, kubeletEndpoint, fmt.Sprintf("%s-%d.sock", pluginEndpointPrefix, time.Now().Unix()))
}
