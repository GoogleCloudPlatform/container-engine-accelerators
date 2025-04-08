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
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia/util"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/fsnotify/fsnotify"
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
	resourceName   = "nvidia.com/gpu"
	pciDevicesRoot = "/sys/bus/pci/devices"
)

// GPUConfig stores the settings used to configure the GPUs on a node.
type GPUConfig struct {
	GPUPartitionSize string
	// MaxTimeSharedClientsPerGPU is the number of the time-shared GPU resources to expose for each physical GPU.
	// Deprecated in favor of GPUSharingConfig.
	MaxTimeSharedClientsPerGPU int
	// GPUSharingConfig informs how GPUs on this node can be shared between containers.
	GPUSharingConfig GPUSharingConfig
	// Xid error codes that will set the node to unhealthy
	HealthCriticalXid []int
}

type GPUSharingConfig struct {
	// GPUSharingStrategy is the type of sharing strategy to enable on this node. Values are "time-sharing" or "mps".
	GPUSharingStrategy gpusharing.GPUSharingStrategy
	// MaxSharedClientsPerGPU is the maximum number of clients that are allowed to share a single GPU.
	MaxSharedClientsPerGPU int
}

func (config *GPUConfig) AddDefaultsAndValidate() error {
	if config.MaxTimeSharedClientsPerGPU > 0 {
		if config.GPUSharingConfig.GPUSharingStrategy != "" || config.GPUSharingConfig.MaxSharedClientsPerGPU > 0 {
			glog.Infof("Both MaxTimeSharedClientsPerGPU and GPUSharingConfig are set, use the value of MaxTimeSharedClientsPerGPU")
		}

		config.GPUSharingConfig.GPUSharingStrategy = gpusharing.TimeSharing
		config.GPUSharingConfig.MaxSharedClientsPerGPU = config.MaxTimeSharedClientsPerGPU
	} else {
		switch config.GPUSharingConfig.GPUSharingStrategy {
		case gpusharing.TimeSharing, gpusharing.MPS:
			if config.GPUSharingConfig.MaxSharedClientsPerGPU <= 0 {
				return fmt.Errorf("MaxSharedClientsPerGPU should be > 0 for time-sharing or mps GPU sharing strategies")
			}
			break
		case gpusharing.Undefined:
			if config.GPUSharingConfig.MaxSharedClientsPerGPU > 0 {
				return fmt.Errorf("GPU sharing strategy needs to be specified when MaxSharedClientsPerGPU > 0")
			}
		default:
			return fmt.Errorf("invalid GPU Sharing strategy: %v, should be one of time-sharing or mps", config.GPUSharingConfig.GPUSharingStrategy)
		}
	}
	gpusharing.SharingStrategy = config.GPUSharingConfig.GPUSharingStrategy
	return nil
}

func (config *GPUConfig) AddHealthCriticalXid() error {
	xidConfig := os.Getenv("XID_CONFIG")
	if len(xidConfig) == 0 {
		glog.Infof("There is no Xid config specified ")
		return nil
	}

	glog.Infof("Detect HealthCriticalXid : %s ", xidConfig)
	xidStrs := strings.Split(xidConfig, ",")
	xidArry := make([]int, len(xidStrs))
	var err error
	for i := range xidArry {
		xidStr := strings.TrimSpace(xidStrs[i])
		xidArry[i], err = strconv.Atoi(xidStr)
		if err != nil {
			return fmt.Errorf("Invalid HealthCriticalXid input : %v", err)
		}
	}
	config.HealthCriticalXid = xidArry
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

func (ngm *nvidiaGPUManager) ListHealthCriticalXid() []int {
	return ngm.gpuConfig.HealthCriticalXid
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
				virtualGPUDevices[virtualDeviceID] = pluginapi.Device{ID: virtualDeviceID, Health: device.Health, Topology: device.Topology}
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

type nvmlOperations interface {
	deviceCount() (int, nvml.Return)
	deviceHandleByIndex(int) (nvml.Device, nvml.Return)
	migDeviceHandleByIndex(nvml.Device, int) (nvml.Device, nvml.Return)
	migMode(nvml.Device) (int, int, nvml.Return)
	minorNumber(nvml.Device) (int, nvml.Return)
	pciInfo(d nvml.Device) (nvml.PciInfo, nvml.Return)
}

// Declare an interface variable for NVML operations.
// This allows the interface to be overriden with mock
// implementations in the tests.
var nvmlDeviceInfo nvmlOperations

// deviceInfo is a struct that implements the nvmlOperations interface.
type deviceInfo struct{}

func (gpuDeviceInfo *deviceInfo) deviceCount() (int, nvml.Return) {
	return nvml.DeviceGetCount()
}

func (gpuDeviceInfo *deviceInfo) deviceHandleByIndex(i int) (nvml.Device, nvml.Return) {
	return nvml.DeviceGetHandleByIndex(i)
}

func (gpuDeviceInfo *deviceInfo) migDeviceHandleByIndex(d nvml.Device, i int) (nvml.Device, nvml.Return) {
	return d.GetMigDeviceHandleByIndex(i)
}

// migMode call's NVML device's GetMigMode() which returns:
// Current mode: The currently active MIG mode
// Pending mode: The MIG mode that will be applied after the next
// GPU reset or system reboot
// Return: NVML return code indicating success or specific error
func (gpuDeviceInfo *deviceInfo) migMode(d nvml.Device) (int, int, nvml.Return) {
	return d.GetMigMode()
}

func (gpuDeviceInfo *deviceInfo) minorNumber(d nvml.Device) (int, nvml.Return) {
	return d.GetMinorNumber()
}

func (gpuDeviceInfo *deviceInfo) pciInfo(d nvml.Device) (nvml.PciInfo, nvml.Return) {
	return d.GetPciInfo()
}

// topology determines the NUMA topology information for a GPU device.
// For MIG-enabled GPUs, it retrieves the NUMA node ID from the parent GPU device.
// For non-MIG GPUs, it gets the NUMA node ID directly from the GPU device.
// Returns a TopologyInfo containing the NUMA node ID if NUMA is enabled, nil
// otherwise.
// Example for a GPU device associated with NUMA node 1
//
//	topologyInfo := &pluginapi.TopologyInfo{
//	    Nodes: []*pluginapi.NUMANode{
//	        {
//	            ID: 1,
//	        },
//	    },
//	}
func topology(d nvml.Device, i int) (*pluginapi.TopologyInfo, error) {
	if nvmlDeviceInfo == nil {
		nvmlDeviceInfo = &deviceInfo{}
	}

	// We only care about currentMode which indicates the current MIG mode state.
	// The pendingMode is ignored as we're only interested in the current
	// operational state, not future configurations.
	currentMode, _, ret := nvmlDeviceInfo.migMode(d)
	if ret != nvml.ERROR_NOT_SUPPORTED {
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("failed to get mig mode: %v", nvml.ErrorString(ret))
		}
	}

	// For GPU topology information: When MIG (Multi-Instance GPU) is enabled, retrieve
	// the NUMA node ID from the parent GPU device. Otherwise, get the NUMA node ID
	// directly from the GPU device itself.
	numaDevice := d
	// A currentMode value of 1 means MIG is currently enabled on the device,
	// while 0 means MIG is disabled.
	if currentMode == 1 {
		parent, ret := nvmlDeviceInfo.migDeviceHandleByIndex(d, i)
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("failed to get mig device handle: %v", nvml.ErrorString(ret))
		}
		numaDevice = parent

	}
	numaEnabled, node, err := numaNode(numaDevice)
	if err != nil {
		return nil, err
	}

	if !numaEnabled {
		return nil, nil
	}

	return &pluginapi.TopologyInfo{
		Nodes: []*pluginapi.NUMANode{
			{
				ID: int64(node),
			},
		},
	}, nil
}

// numaNode retrieves the NUMA node information for a given GPU device.
// It first gets the PCI bus ID from the device, formats it appropriately,
// then reads the NUMA node value from the sysfs filesystem.
func numaNode(d nvml.Device) (numaEnabled bool, numaNode int, err error) {
	if nvmlDeviceInfo == nil {
		nvmlDeviceInfo = &deviceInfo{}
	}
	pciInfo, ret := nvmlDeviceInfo.pciInfo(d)
	if ret != nvml.SUCCESS {
		return false, 0, fmt.Errorf("error getting PCI Bus Info of device with index: %v", ret)
	}

	var bytesT []byte
	for _, b := range pciInfo.BusId {
		if byte(b) == '\x00' {
			break
		}
		bytesT = append(bytesT, byte(b))
	}

	// Discard leading zeros.
	busID := strings.ToLower(strings.TrimPrefix(string(bytesT), "0000"))

	numaNodeFile := fmt.Sprintf("%s/%s/numa_node", pciDevicesRoot, busID)
	glog.Infof("Reading NUMA node information from %q", numaNodeFile)
	b, err := os.ReadFile(numaNodeFile)
	if err != nil {
		return false, 0, fmt.Errorf("failed to read NUMA information from %q file: %v", numaNodeFile, err)
	}

	numaNode, err = strconv.Atoi(string(bytes.TrimSpace(b)))
	if err != nil {
		return false, 0, fmt.Errorf("eror parsing value for NUMA node: %v", err)
	}

	if numaNode < 0 {
		return false, 0, nil
	}

	return true, numaNode, nil
}

// Discovers all NVIDIA GPU devices available on the local node by walking nvidiaGPUManager's devDirectory.
func (ngm *nvidiaGPUManager) discoverGPUs() error {
	if nvmlDeviceInfo == nil {
		nvmlDeviceInfo = &deviceInfo{}
	}

	devicesCount, ret := nvmlDeviceInfo.deviceCount()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("failed to get devices count: %v", nvml.ErrorString(ret))
	}

	for i := 0; i < devicesCount; i++ {
		device, ret := nvmlDeviceInfo.deviceHandleByIndex((i))
		if ret != nvml.SUCCESS {
			return fmt.Errorf("failed to get the device handle for index %d: %v", i, nvml.ErrorString(ret))
		}

		minor, ret := nvmlDeviceInfo.minorNumber(device)
		if ret != nvml.SUCCESS {
			return fmt.Errorf("failed to get the minor number for device with index %d: %v", i, nvml.ErrorString(ret))
		}

		path := fmt.Sprintf("nvidia%d", minor)
		glog.V(3).Infof("Found Nvidia GPU %q\n", path)
		topologyInfo, err := topology(device, i)
		if err != nil {
			glog.Errorln(err)
		}
		ngm.SetDeviceHealth(path, pluginapi.Healthy, topologyInfo)
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
	if ngm.gpuConfig.GPUSharingConfig.GPUSharingStrategy == gpusharing.MPS {
		activeThreadLimit := numDevicesRequested * 100 / ngm.gpuConfig.GPUSharingConfig.MaxSharedClientsPerGPU
		memoryLimitBytes := uint64(numDevicesRequested) * ngm.totalMemPerGPU / uint64(ngm.gpuConfig.GPUSharingConfig.MaxSharedClientsPerGPU)
		return map[string]string{
			mpsThreadLimitEnv: strconv.Itoa(activeThreadLimit),
			// The mpsMemLimitEnv is the GPU memory limit per container, e.g. 0=8192M.
			// 0 represents the device ID which this container resides.
			// Since MPS container can only land in one GPU, it is always device 0 relatively.
			mpsMemLimitEnv: fmt.Sprintf("0=%dM", memoryLimitBytes/(1024*1024)),
		}
	}
	return map[string]string{}
}

// SetDeviceHealth sets the health status for a GPU device or partition if MIG is enabled
func (ngm *nvidiaGPUManager) SetDeviceHealth(name string, health string, topology *pluginapi.TopologyInfo) {
	ngm.devicesMutex.Lock()
	defer ngm.devicesMutex.Unlock()

	reg := regexp.MustCompile(nvidiaDeviceRE)
	if reg.MatchString(name) {
		ngm.devices[name] = pluginapi.Device{ID: name, Health: health, Topology: topology}
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
	count, ret := nvml.DeviceGetCount()
	if ret != nvml.SUCCESS {
		return 0, fmt.Errorf("failed to enumerate devices: %v", nvml.ErrorString(ret))
	}
	if count <= 0 {
		return 0, fmt.Errorf("no GPUs on node, count: %d", count)
	}
	device, ret := nvml.DeviceGetHandleByIndex(0)
	if ret != nvml.SUCCESS {
		return 0, fmt.Errorf("failed to query GPU with nvml: %v", nvml.ErrorString(ret))
	}
	memory, ret := device.GetMemoryInfo()
	if ret != nvml.SUCCESS {
		return 0, fmt.Errorf("failed to get GPU memory: %v", nvml.ErrorString(ret))
	}
	return memory.Total, nil
}

func (ngm *nvidiaGPUManager) Serve(pMountPath, kEndpoint, pluginEndpoint string) {
	registerWithKubelet := false
	// Check if the unix socket device-plugin/kubelet.sock is at the host path.
	kubeletEndpointPath := path.Join(pMountPath, kEndpoint)
	if _, err := os.Stat(kubeletEndpointPath); err == nil {
		glog.Infof("registered with kubelet, will use beta API\n")
		registerWithKubelet = true
	} else {
		glog.Infof("no kubelet.sock to register.\n")
	}

	// Create a watcher to watch /device-plugin directory.
	watcher, _ := util.Files(pMountPath)
	defer watcher.Close()
	glog.Info("Starting filesystem watcher.")

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
					// Restart the device plugin if plugin endpoint file disappears.
					case <-pluginSocketCheck.C:
						if _, err := os.Lstat(pluginEndpointPath); err != nil {
							glog.Infof("stopping device-plugin server at: %s\n", pluginEndpointPath)
							glog.Errorln(err)
							ngm.grpcServer.Stop()
							break statusCheck
						}
					// Restart the device plugin if additional GPU installers.
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
					// Restart the device plugin if kubelet socket gets recreated, which indicates a kubelet restart.
					case event := <-watcher.Events:
						if event.Name == kubeletEndpointPath && event.Op&fsnotify.Create == fsnotify.Create {
							glog.Infof(" %s recreated, stopping device-plugin server", kubeletEndpointPath)
							ngm.grpcServer.Stop()
							break statusCheck
						}
					// Log for any other fs errors and log them. This will not induce a device plugin restart.
					case err := <-watcher.Errors:
						glog.Infof("inotify: %s", err)
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
