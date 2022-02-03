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

package nvidia

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/NVIDIA/gpu-monitoring-tools/bindings/go/nvml"
	"github.com/golang/glog"
	"google.golang.org/grpc"

	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"

	"github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia/gpusharing"
	"github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia/mig"
)

const (
	// All NVIDIA GPUs cards should be mounted with nvidiactl and nvidia-uvm
	// If the driver installed correctly, these two devices will be there.
	nvidiaCtlDevice = "nvidiactl"
	nvidiaUVMDevice = "nvidia-uvm"

	// Optional devices.
	nvidiaUVMToolsDevice = "nvidia-uvm-tools"
	nvidiaModesetDevice  = "nvidia-modeset"

	nvidiaDeviceRE            = `^nvidia[0-9]*$`
	gpuCheckInterval          = 10 * time.Second
	pluginSocketCheckInterval = 1 * time.Second

	nvidiaMpsDir       = "/tmp/nvidia-mps"
	mpsControlBin      = "/usr/local/nvidia/bin/nvidia-cuda-mps-control"
	mpsActiveThreadCmd = "get_default_active_thread_percentage"
	mpsMemLimitEnv     = "CUDA_MPS_PINNED_DEVICE_MEM_LIMIT"
	mpsThreadLimitEnv  = "CUDA_MPS_ACTIVE_THREAD_PERCENTAGE"
)

var (
	resourceName = "nvidia.com/gpu"
)

// GPUConfig stores the settings used to configure the GPUs on a node.
type GPUConfig struct {
	GPUPartitionSize string
	// MaxTimeSharedClientsPerGPU is the number of the time-shared GPU resources to expose for each physical GPU.
	// Deprecated in favor of GPUSharingConfig.
	MaxTimeSharedClientsPerGPU int
	// GPUSharingConfig informs how GPUs on this node can be shared between containers.
	GPUSharingConfig GPUSharingConfig
}

type GPUSharingStrategy string

const (
	Undefined   GPUSharingStrategy = ""
	TimeSharing GPUSharingStrategy = "time-sharing"
	MPS         GPUSharingStrategy = "mps"
)

type GPUSharingConfig struct {
	// GPUSharingStrategy is the type of sharing strategy to enable on this node. Values are "time-sharing" or "mps".
	GPUSharingStrategy GPUSharingStrategy
	// MaxSharedClientsPerGPU is the maximum number of clients that are allowed to share a single GPU.
	MaxSharedClientsPerGPU int
}

func (config *GPUConfig) AddDefaultsAndValidate() error {
	if config.MaxTimeSharedClientsPerGPU > 0 {
		if config.GPUSharingConfig.GPUSharingStrategy != "" || config.GPUSharingConfig.MaxSharedClientsPerGPU > 0 {
			return fmt.Errorf("invalid GPUConfig, only one of MaxTimeSharedClientsPerGPU or GPUSharingConfig should be set")
		}

		config.GPUSharingConfig.GPUSharingStrategy = TimeSharing
		config.GPUSharingConfig.MaxSharedClientsPerGPU = config.MaxTimeSharedClientsPerGPU
	} else {
		switch config.GPUSharingConfig.GPUSharingStrategy {
		case TimeSharing, MPS:
			if config.GPUSharingConfig.MaxSharedClientsPerGPU <= 0 {
				return fmt.Errorf("MaxSharedClientsPerGPU should be > 0 for time-sharing or mps GPU sharing strategies")
			}
			break
		case Undefined:
			if config.GPUSharingConfig.MaxSharedClientsPerGPU > 0 {
				return fmt.Errorf("GPU sharing strategy needs to be specified when MaxSharedClientsPerGPU > 0")
			}
		default:
			return fmt.Errorf("invalid GPU Sharing strategy: %v, should be one of time-sharing or mps", config.GPUSharingConfig.GPUSharingStrategy)

		}
	}
	return nil
}

// nvidiaGPUManager manages nvidia gpu devices.
type nvidiaGPUManager struct {
	devDirectory        string
	mountPaths          []pluginapi.Mount
	defaultDevices      []string
	devices             map[string]pluginapi.Device
	grpcServer          *grpc.Server
	socket              string
	stop                chan bool
	devicesMutex        sync.Mutex
	nvidiaCtlDevicePath string
	nvidiaUVMDevicePath string
	gpuConfig           GPUConfig
	migDeviceManager    mig.DeviceManager
	Health              chan pluginapi.Device
	totalMemPerGPU      uint64 // Total memory available per GPU (in MB)
}

func NewNvidiaGPUManager(devDirectory, procDirectory string, mountPaths []pluginapi.Mount, gpuConfig GPUConfig) *nvidiaGPUManager {
	return &nvidiaGPUManager{

		devDirectory:        devDirectory,
		mountPaths:          mountPaths,
		devices:             make(map[string]pluginapi.Device),
		stop:                make(chan bool),
		nvidiaCtlDevicePath: path.Join(devDirectory, nvidiaCtlDevice),
		nvidiaUVMDevicePath: path.Join(devDirectory, nvidiaUVMDevice),
		gpuConfig:           gpuConfig,
		migDeviceManager:    mig.NewDeviceManager(devDirectory, procDirectory),
		Health:              make(chan pluginapi.Device),
	}
}

// ListPhysicalDevices lists all physical GPU devices (including partitions) available on this node.
func (ngm *nvidiaGPUManager) ListPhysicalDevices() map[string]pluginapi.Device {
	if ngm.gpuConfig.GPUPartitionSize == "" {
		return ngm.devices
	}
	return ngm.migDeviceManager.ListGPUPartitionDevices()
}

// ListDevices lists all GPU devices available on this node.
func (ngm *nvidiaGPUManager) ListDevices() map[string]pluginapi.Device {
	physicalGPUDevices := ngm.ListPhysicalDevices()

	switch {
	case ngm.gpuConfig.GPUSharingConfig.MaxSharedClientsPerGPU > 0:
		virtualGPUDevices := map[string]pluginapi.Device{}
		for _, device := range physicalGPUDevices {
			for i := 0; i < ngm.gpuConfig.GPUSharingConfig.MaxSharedClientsPerGPU; i++ {
				virtualDeviceID := fmt.Sprintf("%s/vgpu%d", device.ID, i)
				// When sharing GPUs, the virtual GPU device will inherit the health status from its underlying physical GPU device.
				virtualGPUDevices[virtualDeviceID] = pluginapi.Device{ID: virtualDeviceID, Health: device.Health}
			}
		}
		return virtualGPUDevices
	default:
		return physicalGPUDevices
	}
}

// DeviceSpec returns the device spec that inclues list of devices to allocate for a deviceID.
func (ngm *nvidiaGPUManager) DeviceSpec(deviceID string) ([]pluginapi.DeviceSpec, error) {
	deviceSpecs := make([]pluginapi.DeviceSpec, 0)
	// With GPU sharing, the input deviceID will be a virtual Device ID.
	// We need to map it to the corresponding physical device ID.
	if ngm.gpuConfig.GPUSharingConfig.MaxSharedClientsPerGPU > 0 {
		physicalDeviceID, err := gpusharing.VirtualToPhysicalDeviceID(deviceID)
		if err != nil {
			return nil, err
		}
		deviceID = physicalDeviceID
	}
	if ngm.gpuConfig.GPUPartitionSize == "" {
		dev, ok := ngm.devices[deviceID]
		if !ok {
			return deviceSpecs, fmt.Errorf("invalid allocation request with non-existing device %s", deviceID)
		}
		if dev.Health != pluginapi.Healthy {
			return deviceSpecs, fmt.Errorf("invalid allocation request with unhealthy device %s", deviceID)
		}
		deviceSpecs = append(deviceSpecs, pluginapi.DeviceSpec{
			HostPath:      path.Join(ngm.devDirectory, deviceID),
			ContainerPath: path.Join(ngm.devDirectory, deviceID),
			Permissions:   "mrw",
		})
		return deviceSpecs, nil
	}
	return ngm.migDeviceManager.DeviceSpec(deviceID)
}

// Discovers all NVIDIA GPU devices available on the local node by walking nvidiaGPUManager's devDirectory.
func (ngm *nvidiaGPUManager) discoverGPUs() error {
	reg := regexp.MustCompile(nvidiaDeviceRE)
	files, err := ioutil.ReadDir(ngm.devDirectory)
	if err != nil {
		return err
	}
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		if reg.MatchString(f.Name()) {
			glog.V(3).Infof("Found Nvidia GPU %q\n", f.Name())
			ngm.SetDeviceHealth(f.Name(), pluginapi.Healthy)
		}
	}
	return nil
}

func (ngm *nvidiaGPUManager) hasAdditionalGPUsInstalled() bool {
	ngm.devicesMutex.Lock()
	originalDeviceCount := len(ngm.devices)
	ngm.devicesMutex.Unlock()
	deviceCount, err := ngm.discoverNumGPUs()
	if err != nil {
		glog.Errorln(err)
		return false
	}
	if deviceCount > originalDeviceCount {
		glog.Infof("Found %v GPUs, while only %v are registered. Stopping device-plugin server.", deviceCount, originalDeviceCount)
		return true
	}
	return false
}

func (ngm *nvidiaGPUManager) discoverNumGPUs() (int, error) {
	reg := regexp.MustCompile(nvidiaDeviceRE)
	deviceCount := 0
	files, err := ioutil.ReadDir(ngm.devDirectory)
	if err != nil {
		return deviceCount, err
	}
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		if reg.MatchString(f.Name()) {
			deviceCount++
		}
	}
	return deviceCount, nil
}

// isMpsHealthy checks whether MPS control daemon is running and healhty on the node.
func (ngm *nvidiaGPUManager) isMpsHealthy() error {
	var out bytes.Buffer
	reader, writer := io.Pipe()
	defer writer.Close()
	defer reader.Close()

	mpsCmd := exec.Command(mpsControlBin)
	mpsCmd.Stdin = reader
	mpsCmd.Stdout = &out

	err := mpsCmd.Start()
	if err != nil {
		return fmt.Errorf("failed to start NVIDIA MPS health check command: %v", err)
	}

	writer.Write([]byte(mpsActiveThreadCmd))
	writer.Close()

	err = mpsCmd.Wait()
	if err != nil {
		return fmt.Errorf("failed to health check NVIDIA MPS: %v", err)
	}

	reader.Close()
	glog.Infof("MPS is healthy, active thread percentage = %s", out.String())
	return nil
}

func (ngm *nvidiaGPUManager) Envs(numDevicesRequested int) map[string]string {
	if ngm.gpuConfig.GPUSharingConfig.GPUSharingStrategy == MPS {
		activeThreadLimit := numDevicesRequested * 100 / ngm.gpuConfig.GPUSharingConfig.MaxSharedClientsPerGPU
		memoryLimit := uint64(numDevicesRequested) * ngm.totalMemPerGPU / uint64(ngm.gpuConfig.GPUSharingConfig.MaxSharedClientsPerGPU)

		return map[string]string{
			mpsThreadLimitEnv: strconv.Itoa(activeThreadLimit),
			mpsMemLimitEnv:    fmt.Sprintf("%dMB", memoryLimit),
		}

	}
	return map[string]string{}
}

// SetDeviceHealth sets the health status for a GPU device or partition if MIG is enabled
func (ngm *nvidiaGPUManager) SetDeviceHealth(name string, health string) {
	ngm.devicesMutex.Lock()
	defer ngm.devicesMutex.Unlock()

	reg := regexp.MustCompile(nvidiaDeviceRE)
	if reg.MatchString(name) {
		ngm.devices[name] = pluginapi.Device{ID: name, Health: health}
	} else {
		ngm.migDeviceManager.SetDeviceHealth(name, health)
	}
}

// Checks if the two nvidia paths exist. Could be used to verify if the driver
// has been installed correctly
func (ngm *nvidiaGPUManager) CheckDevicePaths() error {
	if _, err := os.Stat(ngm.nvidiaCtlDevicePath); err != nil {
		return err
	}

	if _, err := os.Stat(ngm.nvidiaUVMDevicePath); err != nil {
		return err
	}
	return nil
}

// Discovers Nvidia GPU devices and sets up device access environment.
func (ngm *nvidiaGPUManager) Start() error {
	ngm.defaultDevices = []string{ngm.nvidiaCtlDevicePath, ngm.nvidiaUVMDevicePath}

	nvidiaModesetDevicePath := path.Join(ngm.devDirectory, nvidiaModesetDevice)
	if _, err := os.Stat(nvidiaModesetDevicePath); err == nil {
		ngm.defaultDevices = append(ngm.defaultDevices, nvidiaModesetDevicePath)
	}

	nvidiaUVMToolsDevicePath := path.Join(ngm.devDirectory, nvidiaUVMToolsDevice)
	if _, err := os.Stat(nvidiaUVMToolsDevicePath); err == nil {
		ngm.defaultDevices = append(ngm.defaultDevices, nvidiaUVMToolsDevicePath)
	}

	if err := ngm.discoverGPUs(); err != nil {
		return err
	}
	if ngm.gpuConfig.GPUPartitionSize != "" {
		if err := ngm.migDeviceManager.Start(ngm.gpuConfig.GPUPartitionSize); err != nil {
			return fmt.Errorf("failed to start mig device manager: %v", err)
		}
	}

	if ngm.gpuConfig.GPUSharingConfig.GPUSharingStrategy == "mps" {
		if err := ngm.isMpsHealthy(); err != nil {
			return fmt.Errorf("NVIDIA MPS is not running on this node: %v", err)
		}
		ngm.mountPaths = append(ngm.mountPaths, pluginapi.Mount{HostPath: nvidiaMpsDir, ContainerPath: nvidiaMpsDir, ReadOnly: false})
		var err error
		ngm.totalMemPerGPU, err = totalMemPerGPU()
		if err != nil {
			return fmt.Errorf("failed to query total memory available per GPU: %v", err)
		}
	}

	return nil
}

// totalMemPerGPU returns the GPU memory available on each GPU device.
func totalMemPerGPU() (uint64, error) {
	count, err := nvml.GetDeviceCount()
	if err != nil {
		return 0, fmt.Errorf("failed to enumerate devices: %v", err)
	}
	if count <= 0 {
		return 0, fmt.Errorf("no GPUs on node, count: %d", count)
	}
	device, err := nvml.NewDevice(0)
	if err != nil {
		return 0, fmt.Errorf("failed to query GPU with nvml: %v", err)
	}
	return *device.Memory, nil
}

func (ngm *nvidiaGPUManager) Serve(pMountPath, kEndpoint, pluginEndpoint string) {
	registerWithKubelet := false
	if _, err := os.Stat(path.Join(pMountPath, kEndpoint)); err == nil {
		glog.Infof("will use alpha API\n")
		registerWithKubelet = true
	} else {
		glog.Infof("will use beta API\n")
	}

	for {
		select {
		case <-ngm.stop:
			close(ngm.stop)
			return
		default:
			{
				pluginEndpointPath := path.Join(pMountPath, pluginEndpoint)
				glog.Infof("starting device-plugin server at: %s\n", pluginEndpointPath)
				lis, err := net.Listen("unix", pluginEndpointPath)
				if err != nil {
					glog.Fatalf("starting device-plugin server failed: %v", err)
				}
				ngm.socket = pluginEndpointPath
				ngm.grpcServer = grpc.NewServer()

				// Registers the supported versions of service.
				pluginbeta := &pluginServiceV1Beta1{ngm: ngm}
				pluginbeta.RegisterService()

				var wg sync.WaitGroup
				wg.Add(1)
				// Starts device plugin service.
				go func() {
					defer wg.Done()
					// Blocking call to accept incoming connections.
					err := ngm.grpcServer.Serve(lis)
					glog.Errorf("device-plugin server stopped serving: %v", err)
				}()

				if registerWithKubelet {
					// Wait till the grpcServer is ready to serve services.
					for len(ngm.grpcServer.GetServiceInfo()) <= 0 {
						time.Sleep(1 * time.Second)
					}
					glog.Infoln("device-plugin server started serving")
					// Registers with Kubelet.
					err = RegisterWithV1Beta1Kubelet(path.Join(pMountPath, kEndpoint), pluginEndpoint, resourceName)
					if err != nil {
						ngm.grpcServer.Stop()
						wg.Wait()
						glog.Fatal(err)
					}
					glog.Infoln("device-plugin registered with the kubelet")
				}

				// This is checking if the plugin socket was deleted
				// and also if there are additional GPU devices installed.
				// If so, stop the grpc server and start the whole thing again.
				gpuCheck := time.NewTicker(gpuCheckInterval)
				pluginSocketCheck := time.NewTicker(pluginSocketCheckInterval)
				defer gpuCheck.Stop()
				defer pluginSocketCheck.Stop()
			statusCheck:
				for {
					select {
					case <-pluginSocketCheck.C:
						if _, err := os.Lstat(pluginEndpointPath); err != nil {
							glog.Infof("stopping device-plugin server at: %s\n", pluginEndpointPath)
							glog.Errorln(err)
							ngm.grpcServer.Stop()
							break statusCheck
						}
					case <-gpuCheck.C:
						if ngm.hasAdditionalGPUsInstalled() {
							ngm.grpcServer.Stop()
							for {
								err := ngm.discoverGPUs()
								if err == nil {
									break statusCheck
								}
							}
						}

					}
				}
				wg.Wait()
			}
		}
	}
}

func (ngm *nvidiaGPUManager) Stop() error {
	glog.Infof("removing device plugin socket %s\n", ngm.socket)
	if err := os.Remove(ngm.socket); err != nil && !os.IsNotExist(err) {
		return err
	}
	ngm.stop <- true
	<-ngm.stop
	close(ngm.Health)
	return nil
}
