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
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"time"

	gpumanager "github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia"
	healthcheck "github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia/health_check"
	"github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia/metrics"
	util "github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia/util"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/golang/glog"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

const (
	// Device plugin settings.
	kubeletEndpoint      = "kubelet.sock"
	pluginEndpointPrefix = "nvidiaGPU"
	devDirectory         = "/dev"
	nodeNameEnv          = "NODE_NAME"
	lockFilePath         = "/device-plugin/tpu-device-plugin.lock"

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
	enableFlockWait                = flag.Bool("enable-flock-wait", false, "If true, the device plugin will wait until the old device plugin release the lock")
	gpuMetricsPort                 = flag.Int("gpu-metrics-port", 2112, "Port on which GPU metrics for containers are exposed")
	gpuMetricsCollectionIntervalMs = flag.Int("gpu-metrics-collection-interval", 30000, "Collection interval (in milli seconds) for container GPU metrics")
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

	err = gpuConfig.AddDefaultsAndValidate()
	if err != nil {
		return gpumanager.GPUConfig{}, err
	}
	return gpuConfig, nil
}

func main() {
	flag.Parse()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // Ensure the context is canceled when main exits
	glog.Infoln("device-plugin started")
	mountPaths := []pluginapi.Mount{
		{HostPath: *hostPathPrefix, ContainerPath: *containerPathPrefix, ReadOnly: true},
		{HostPath: *hostVulkanICDPathPrefix, ContainerPath: *containerVulkanICDPathPrefix, ReadOnly: true}}

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
	err := gpuConfig.AddHealthCriticalXid()
	if err != nil {
		glog.Infof("Failed to Add HealthCriticalXid : %v", err)
	}

	glog.Infof("Using gpu config: %v", gpuConfig)
	ngm := gpumanager.NewNvidiaGPUManager(devDirectory, procDirectory, mountPaths, gpuConfig)

	// Retry until nvidiactl and nvidia-uvm are detected. This is required
	// because Nvidia drivers may not be installed initially.
	for {
		err := ngm.CheckDevicePaths()
		if err == nil {
			break
		}
		// Use non-default level to avoid log spam.
		glog.V(3).Infof("nvidiaGPUManager.CheckDevicePaths() failed: %v", err)
		time.Sleep(5 * time.Second)
	}

	if ret := nvml.Init(); ret != nvml.SUCCESS {
		glog.Fatalf("failed to initialize nvml: %v", nvml.ErrorString(ret))
	}
	defer nvml.Shutdown()

	for {
		err := ngm.Start()
		if err == nil {
			break
		}

		glog.Errorf("failed to start GPU device manager: %v", err)
		time.Sleep(5 * time.Second)
	}

	if *enableContainerGPUMetrics {
		if gpuConfig.GPUPartitionSize != "" {
			glog.Info("Using multi-instance GPU, metrics are not supported.")
		} else {
			glog.Infof("Starting metrics server on port: %d, endpoint path: %s, collection frequency: %d", *gpuMetricsPort, "/metrics", *gpuMetricsCollectionIntervalMs)
			metricServer := metrics.NewMetricServer(*gpuMetricsCollectionIntervalMs, *gpuMetricsPort, "/metrics")
			err := metricServer.Start()
			if err != nil {
				glog.Infof("Failed to start metric server: %v", err)
				return
			}
			defer metricServer.Stop()
		}
	}

	if *enableHealthMonitoring {
		kubeClient, err := util.BuildKubeClient()
		if err != nil {
			glog.Infof("Failed to build kube client: %v", err)
			return
		}
		hc := healthcheck.NewGPUHealthChecker(ngm.ListPhysicalDevices(), ngm.Health, ngm.ListHealthCriticalXid(), kubeClient)
		if err := hc.Start(); err != nil {
			glog.Infof("Failed to start GPU Health Checker: %v", err)
			return
		}
		defer hc.Stop()

	}

	if *enableFlockWait {
		kubeClient, err := util.BuildKubeClient()
		if err != nil {
			glog.Infof("Failed to build kube client: %v", err)
			return
		}
		nodeName, err := util.GetEnv(nodeNameEnv)
		if err != nil {
			glog.Infof("Failed to get node name from environment variable %s: %v", nodeNameEnv, err)
			return
		}
		watchfunc := func(options metav1.ListOptions) (watch.Interface, error) {
			return kubeClient.CoreV1().Nodes().Watch(ctx, metav1.ListOptions{
				FieldSelector: "metadata.name=" + nodeName,
			})
		}
		if err := util.SafelyUsingFlockWait(ctx, lockFilePath, watchfunc, util.CheckLockFileExists, util.UseRetryWatch); err != nil {
			glog.Errorf("Failed to safely use flock wait, exiting... %v", err)
			os.Exit(1)
		}
	}

	ngm.Serve(*pluginMountPath, kubeletEndpoint, fmt.Sprintf("%s-%d.sock", pluginEndpointPrefix, time.Now().Unix()))
}
