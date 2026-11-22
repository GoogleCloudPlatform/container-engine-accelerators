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
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"io/ioutil"
	"os"
	"strconv"
	"strings"
	"time"

	gpumanager "github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia"
	"github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia/gpusharing"
	healthcheck "github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia/health_check"
	"github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia/metrics"
	"github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia/mig"
	"github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia/reset"
	util "github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia/util"
	versionvisibility "github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia/version_visibility"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/golang/glog"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/client-go/kubernetes"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

const (
	// Device plugin settings.
	kubeletEndpoint      = "kubelet.sock"
	pluginEndpointPrefix = "nvidiaGPU"
	devDirectory         = "/dev"
	// Proc directory is used to lookup the access files for each GPU partition.
	procDirectory = "/proc"

	defaultHealthSendTimeout = 5 * time.Second
)

var (
	hostPathPrefix                 = flag.String("host-path", "/home/kubernetes/bin/nvidia", "Path on the host that contains nvidia libraries. This will be mounted inside the container as '-container-path'")
	containerPathPrefix            = flag.String("container-path", "/usr/local/nvidia", "Path on the container that mounts '-host-path'")
	hostVulkanICDPathPrefix        = flag.String("host-vulkan-icd-path", "/home/kubernetes/bin/nvidia/vulkan/icd.d", "Path on the host that contains the Nvidia Vulkan installable client driver. This will be mounted inside the container as '-container-vulkan-icd-path'")
	containerVulkanICDPathPrefix   = flag.String("container-vulkan-icd-path", "/etc/vulkan/icd.d", "Path on the container that mounts '-host-vulkan-icd-path'")
	pluginMountPath                = flag.String("plugin-directory", "/device-plugin", "The directory path to create plugin socket")
	enableContainerGPUMetrics      = flag.Bool("enable-container-gpu-metrics", false, "If true, the device plugin will expose GPU metrics for containers with allocated GPU")
	enableHealthMonitoring         = flag.Bool("enable-health-monitoring", false, "If true, the device plugin will detect critical Xid errors and mark the GPUs unallocatable")
	enableGPUReset                 = flag.Bool("enable-gpu-reset", false, "If true, the device plugin will watch for the perform-gpu-reset node label and execute graceful GPU resets")
	gpuMetricsPort                 = flag.Int("gpu-metrics-port", 2112, "Port on which GPU metrics for containers are exposed")
	gpuMetricsCollectionIntervalMs = flag.Int("gpu-metrics-collection-interval", 30000, "Collection interval (in milli seconds) for container GPU metrics")
	gpuConfigFile                  = flag.String("gpu-config", "/etc/nvidia/gpu_config.json", "File with GPU configurations for device plugin")
	gpuFractionDivisorFile         = flag.String("gpu-fraction-divisor-file", "/etc/nvidia/gpu_fraction_divisor.txt", "File containing the divisor for vGPU machine shapes")
	publishDriverVersion           = flag.Bool("publish-driver-version", false, "If true, the device plugin will publish NVIDIA driver versions to the Kubernetes Node annotation")
)

type gpuDeviceManager interface {
	PhysicalGPUIDs() []string
	ListPhysicalDevices() map[string]pluginapi.Device
	DeviceHealth(name string) string
	SetDeviceHealth(name string, health string, topology *pluginapi.TopologyInfo)
	MIGDeviceManager() *mig.DeviceManager
}

type ngmDevices struct {
	ngm         gpuDeviceManager
	healthCh    chan<- pluginapi.Device
	sendTimeout time.Duration
}

func newNGMDevices(ngm gpuDeviceManager, healthCh chan<- pluginapi.Device) *ngmDevices {
	return &ngmDevices{
		ngm:         ngm,
		healthCh:    healthCh,
		sendTimeout: defaultHealthSendTimeout,
	}
}

func (d *ngmDevices) Physical() []reset.PhysicalGPU {
	if d == nil || d.ngm == nil {
		return nil
	}
	ids := d.ngm.PhysicalGPUIDs()
	out := make([]reset.PhysicalGPU, len(ids))
	for i, id := range ids {
		out[i] = reset.PhysicalGPU(id)
	}
	return out
}

func (d *ngmDevices) Health(id string) reset.Health {
	if d == nil || d.ngm == nil {
		return reset.Unhealthy
	}
	return reset.Health(d.ngm.DeviceHealth(id))
}

func (d *ngmDevices) SetHealth(id string, h reset.Health) error {
	if d == nil || d.ngm == nil {
		return errors.New("ngmDevices: GPU device manager is nil")
	}
	d.ngm.SetDeviceHealth(id, string(h), nil)
	if d.healthCh == nil {
		return errors.New("ngmDevices: device plugin Health channel is nil")
	}
	timeout := d.sendTimeout
	if timeout <= 0 {
		timeout = defaultHealthSendTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case d.healthCh <- pluginapi.Device{ID: id, Health: string(h)}:
		return nil
	case <-timer.C:
		return fmt.Errorf("timed out after %v sending health update (%s=%s) to device plugin Health channel", timeout, id, h)
	}
}

type stoppableMonitor interface {
	Stop()
	Start() error
}

type pluginMonitors struct {
	deviceLister  func() map[string]pluginapi.Device
	healthChecker stoppableMonitor
	metricServer  stoppableMonitor
}

func newPluginMonitors(ngm gpuDeviceManager, hc *healthcheck.GPUHealthChecker, ms *metrics.MetricServer) *pluginMonitors {
	var m pluginMonitors
	if ngm != nil {
		m.deviceLister = ngm.ListPhysicalDevices
	}
	if hc != nil {
		m.healthChecker = hc
	}
	if ms != nil {
		m.metricServer = ms
	}
	return &m
}

func (m *pluginMonitors) RefreshDevices() {
	if m == nil || m.deviceLister == nil || m.healthChecker == nil {
		return
	}
	if rf, ok := m.healthChecker.(interface {
		RefreshDevices(map[string]pluginapi.Device)
	}); ok {
		rf.RefreshDevices(m.deviceLister())
	}
}

func (m *pluginMonitors) Stop() {
	if m == nil {
		return
	}
	if m.healthChecker != nil {
		m.healthChecker.Stop()
	}
	if m.metricServer != nil {
		m.metricServer.Stop()
	}
}

func (m *pluginMonitors) Start() error {
	if m == nil {
		return nil
	}
	var errs []error
	if m.healthChecker != nil {
		m.RefreshDevices()
		if err := m.healthChecker.Start(); err != nil {
			errs = append(errs, fmt.Errorf("start GPU health checker: %w", err))
		}
	}
	if m.metricServer != nil {
		if err := m.metricServer.Start(); err != nil {
			errs = append(errs, fmt.Errorf("start GPU metric server: %w", err))
		}
	}
	return errors.Join(errs...)
}

func resetConfigFromGPUConfig(gpuConfig gpumanager.GPUConfig) reset.Config {
	sharing := gpuConfig.GPUSharingConfig.GPUSharingStrategy
	maxClients := gpuConfig.GPUSharingConfig.MaxSharedClientsPerGPU
	if gpuConfig.MaxTimeSharedClientsPerGPU > 0 {
		sharing = gpusharing.TimeSharing
		maxClients = gpuConfig.MaxTimeSharedClientsPerGPU
	}
	return reset.Config{
		PartitionSize:          gpuConfig.GPUPartitionSize,
		Sharing:                sharing,
		MaxSharedClientsPerGPU: maxClients,
		FractionDivisor:        gpuConfig.GPUFractionDivisor,
	}
}

func newResetWatcher(
	kubeClient kubernetes.Interface,
	nodeName string,
	gpuConfig gpumanager.GPUConfig,
	ngm gpuDeviceManager,
	healthCh chan<- pluginapi.Device,
	monitors reset.Monitors,
	reg prometheus.Registerer,
	nvmlAdapter reset.NVML,
) (*reset.Watcher, *reset.Runner, error) {
	if kubeClient == nil {
		return nil, nil, errors.New("kubernetes client is nil")
	}
	if nodeName == "" {
		return nil, nil, errors.New("NODE_NAME environment variable not set")
	}
	if ngm == nil {
		return nil, nil, errors.New("GPU device manager is nil")
	}

	if nvmlAdapter == nil {
		nvmlAdapter = reset.NewNVML()
	}
	cfg := resetConfigFromGPUConfig(gpuConfig)
	mode := reset.NewMode(
		cfg,
		reset.WithMIGDevices(ngm.MIGDeviceManager()),
		reset.WithNVML(nvmlAdapter),
	)
	devices := newNGMDevices(ngm, healthCh)
	runner := reset.NewRunner(cfg, mode, devices, monitors, nvmlAdapter)
	if reg != nil {
		if err := runner.Register(reg); err != nil {
			glog.Warningf("Failed to register GPU reset metrics: %v", err)
		}
	}
	nodeWriter := reset.NewNodeWriter(kubeClient, nodeName)
	watcher := reset.NewWatcher(kubeClient, nodeName, runner, nodeWriter)
	return watcher, runner, nil
}

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

// parseGPUFractionDivisor reads the GPU fraction divisor from the given file and returns it.
// All GPUs have a default fraction of 1, any integer greater implies its a vGPU machine type.
func parseGPUFractionDivisor(gpuFractionDivisorFile string) (int, error) {
	if gpuFractionDivisorFile == "" {
		return 1, fmt.Errorf("GPU fraction divisor file not defined at %v, defaulting to 1", gpuFractionDivisorFile)
	}

	file, err := os.ReadFile(gpuFractionDivisorFile)
	if errors.Is(err, fs.ErrNotExist) {
		return 1, fmt.Errorf("GPU fraction divisor file not found at %v, defaulting to 1", gpuFractionDivisorFile)
	} else if err != nil {
		return 1, fmt.Errorf("Failed to read GPU fraction divisor file at %v, defaulting to 1: %v", gpuFractionDivisorFile, err)
	}

	gpuFractionDivisorStr := strings.TrimSpace(string(file))
	glog.Infof("GPU fraction divisor file: %s, fraction = %s", gpuFractionDivisorFile, gpuFractionDivisorStr)
	fractionDivisor, err := strconv.Atoi(gpuFractionDivisorStr)
	if err != nil {
		return 1, fmt.Errorf("Failed to parse GPU fraction divisor file at %v, defaulting to 1: %v", gpuFractionDivisorFile, err)
	}
	if fractionDivisor <= 1 {
		return 1, fmt.Errorf("Provided GPU fraction value is less than or equal to 1, defaulting to 1: %v", fractionDivisor)
	}

	return fractionDivisor, nil
}

func main() {
	flag.Parse()
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

	gpuFractionDivisor, err := parseGPUFractionDivisor(*gpuFractionDivisorFile)
	if err != nil {
		// This is NOT a fatal error, but the user should be aware.
		glog.Errorf("Encountered error while parsing GPU fraction divisor file: %v", err)
	}
	gpuConfig.GPUFractionDivisor = gpuFractionDivisor

	err = gpuConfig.AddHealthCriticalXid()
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

	var metricServer *metrics.MetricServer
	if *enableContainerGPUMetrics {
		if gpuConfig.GPUPartitionSize != "" {
			glog.Info("Using multi-instance GPU, metrics are not supported.")
		} else {
			glog.Infof("Starting metrics server on port: %d, endpoint path: %s, collection frequency: %d", *gpuMetricsPort, "/metrics", *gpuMetricsCollectionIntervalMs)
			metricServer = metrics.NewMetricServer(*gpuMetricsCollectionIntervalMs, *gpuMetricsPort, "/metrics")
			err := metricServer.Start()
			if err != nil {
				glog.Infof("Failed to start metric server: %v", err)
				return
			}
			defer metricServer.Stop()
		}
	}

	var hc *healthcheck.GPUHealthChecker
	if *enableHealthMonitoring {
		kubeClient, err := util.BuildKubeClient()
		if err != nil {
			glog.Infof("Failed to build kube client: %v", err)
			return
		}
		hc = healthcheck.NewGPUHealthChecker(ngm.ListPhysicalDevices(), ngm.Health, ngm.ListHealthCriticalXid(), kubeClient)
		if err := hc.Start(); err != nil {
			glog.Infof("Failed to start GPU Health Checker: %v", err)
			return
		}
		defer hc.Stop()
	}

	if *enableGPUReset {
		kubeClient, err := util.BuildKubeClient()
		if err != nil {
			glog.Infof("Failed to build kube client for GPU reset: %v", err)
			return
		}
		nodeName := os.Getenv("NODE_NAME")
		monitors := newPluginMonitors(ngm, hc, metricServer)
		watcher, _, err := newResetWatcher(kubeClient, nodeName, gpuConfig, ngm, ngm.Health, monitors, metricServer.Registerer(), reset.NewNVML())
		if err != nil {
			glog.Errorf("Failed to initialize GPU reset watcher: %v", err)
			return
		}
		resetCtx, cancelReset := context.WithCancel(context.Background())
		defer cancelReset()
		go func() {
			if err := watcher.Start(resetCtx); err != nil {
				glog.Errorf("GPU reset watcher stopped with error: %v", err)
			}
		}()
	}

	if *publishDriverVersion {
		glog.Infoln("Publishing NVIDIA driver version annotations to Node")
		kubeClient, err := util.BuildKubeClient()
		if err != nil {
			glog.Warningf("Failed to build kube client for annotating: %v", err)
		} else {
			nodeName := os.Getenv("NODE_NAME")
			if nodeName == "" {
				glog.Warning("NODE_NAME environment variable not set, skipping publishing driver version annotations")
			} else {
				apiTimeout := 10 * time.Second
				ctx, cancel := context.WithTimeout(context.Background(), apiTimeout)
				defer cancel()
				go func() {
					err := versionvisibility.PublishDriverVersionAnnotations(ctx, kubeClient, nodeName)
					if err != nil {
						glog.Errorf("Failed to publish driver version annotations: %v", err)
					} else {
						glog.Info("Successfully published NVIDIA driver version annotations")
					}
				}()
			}
		}
	}

	ngm.Serve(*pluginMountPath, kubeletEndpoint, fmt.Sprintf("%s-%d.sock", pluginEndpointPrefix, time.Now().Unix()))
}
