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
	"net"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/mindprince/gonvml"
	"golang.org/x/net/context"
	"google.golang.org/grpc"

	pluginapi "k8s.io/kubernetes/pkg/kubelet/apis/deviceplugin/v1alpha"
)

const (
	// All NVIDIA GPUs cards should be mounted with nvidiactl and nvidia-uvm
	// If the driver installed correctly, these two devices will be there.
	nvidiaCtlDevice = "/dev/nvidiactl"
	nvidiaUVMDevice = "/dev/nvidia-uvm"
	// Optional device.
	nvidiaUVMToolsDevice = "/dev/nvidia-uvm-tools"
	devDirectory         = "/dev"
	nvidiaDeviceRE       = `^nvidia[0-9]*$`

	// Device plugin settings.
	pluginMountPath      = "/device-plugin"
	kubeletEndpoint      = "kubelet.sock"
	pluginEndpointPrefix = "nvidiaGPU"
	resourceName         = "nvidia.com/gpu"
)

var (
	hostPathPrefix      = flag.String("host-path", "/home/kubernetes/bin/nvidia", "Path on the host that contains nvidia libraries. This will be mounted inside the container as '-container-path'")
	containerPathPrefix = flag.String("container-path", "/usr/local/nvidia", "Path on the container that mounts '-host-path'")
)

// nvidiaGPUManager manages nvidia gpu devices.
type nvidiaGPUManager struct {
	defaultDevices []string
	devices        map[string]pluginapi.Device
	grpcServer     *grpc.Server
}

func NewNvidiaGPUManager() *nvidiaGPUManager {
	return &nvidiaGPUManager{
		devices: make(map[string]pluginapi.Device),
	}
}

// Discovers all NVIDIA GPU devices available on the local node by walking `/dev` directory.
func (ngm *nvidiaGPUManager) discoverGPUs() error {

	err := gonvml.Initialize()
	if err != nil {
		return err
	}
	deviceCount, err := gonvml.DeviceCount()
	if err != nil {
		return err
	}
	for i := 0; i < int(deviceCount); i++ {
		dev, err := gonvml.DeviceHandleByIndex(uint(i))
		if err != nil {
			glog.Errorf("\tDeviceHandleByIndex() error: %v\n", err)
			return err
		}

		minorNumber, err := dev.MinorNumber()
		if err != nil {
			glog.Errorf("\tdev.MinorNumber() error: %v\n", err)
			return err
		}
		devName := fmt.Sprintf("nvidia%d", uint(minorNumber))
		glog.Infof("Found Nvidia GPU %q\n", devName)
		ngm.devices[devName] = pluginapi.Device{devName, pluginapi.Healthy}
	}
	return nil
}

// TODO: This would not be necessary if we could store this value in a deviceplugin
// field like actualDevice
func convertToMinorNumber(DeviceName string) (uint, error) {
	devString := strings.TrimPrefix(DeviceName, "nvidia")
	minorNumber, err := strconv.ParseUint(devString, 10, 64)
	if err != nil {
		return 0, err
	}
	return uint(minorNumber), nil
}

// TODO: Consider modifying ListandWatch to concurrently check state
func (ngm *nvidiaGPUManager) GetDeviceState(DeviceName string) (string, error) {
	reg := regexp.MustCompile(nvidiaDeviceRE)
	if reg.MatchString(DeviceName) {
		minorNumber, err := convertToMinorNumber(DeviceName)
		if err != nil {
			return pluginapi.Unhealthy, err
		}
		_, err = gonvml.DeviceHandleByIndex(minorNumber)
		if err != nil {
			glog.Errorf("\tCould not find GPU device %v\n", DeviceName)
			return pluginapi.Unhealthy, err
		}
		return pluginapi.Healthy, nil
	}
	return pluginapi.Healthy, nil
}

// Discovers Nvidia GPU devices and sets up device access environment.
func (ngm *nvidiaGPUManager) Start() error {
	if _, err := os.Stat(nvidiaCtlDevice); err != nil {
		return err
	}

	if _, err := os.Stat(nvidiaUVMDevice); err != nil {
		return err
	}

	ngm.defaultDevices = []string{nvidiaCtlDevice, nvidiaUVMDevice}

	if _, err := os.Stat(nvidiaUVMToolsDevice); err == nil {
		ngm.defaultDevices = append(ngm.defaultDevices, nvidiaUVMToolsDevice)
	}

	if err := ngm.discoverGPUs(); err != nil {
		return err
	}

	return nil
}

// Act as a grpc client and register with the kubelet.
func Register(kubeletEndpoint, pluginEndpoint, resourceName string) error {
	conn, err := grpc.Dial(kubeletEndpoint, grpc.WithInsecure(),
		grpc.WithDialer(func(addr string, timeout time.Duration) (net.Conn, error) {
			return net.DialTimeout("unix", addr, timeout)
		}))
	if err != nil {
		return fmt.Errorf("device-plugin: cannot connect to kubelet service: %v", err)
	}
	defer conn.Close()
	client := pluginapi.NewRegistrationClient(conn)

	request := &pluginapi.RegisterRequest{
		Version:      pluginapi.Version,
		Endpoint:     pluginEndpoint,
		ResourceName: resourceName,
	}

	if _, err = client.Register(context.Background(), request); err != nil {
		return fmt.Errorf("device-plugin: cannot register to kubelet service: %v", err)
	}
	return nil
}

// Implements DevicePlugin service functions
func (ngm *nvidiaGPUManager) ListAndWatch(emtpy *pluginapi.Empty, stream pluginapi.DevicePlugin_ListAndWatchServer) error {
	glog.Infoln("device-plugin: ListAndWatch start")
	changed := true
	for {
		for id, dev := range ngm.devices {
			state, err := ngm.GetDeviceState(id)
			if err != nil {
				return err
			}
			if dev.Health != state {
				changed = true
				dev.Health = state
				ngm.devices[id] = dev
			}
		}
		if changed {
			resp := new(pluginapi.ListAndWatchResponse)
			for _, dev := range ngm.devices {
				resp.Devices = append(resp.Devices, &pluginapi.Device{dev.ID, dev.Health})
			}
			glog.Infof("ListAndWatch: send devices %v\n", resp)
			if err := stream.Send(resp); err != nil {
				glog.Errorf("device-plugin: cannot update device states: %v\n", err)
				ngm.grpcServer.Stop()
				return err
			}
		}
		changed = false
		time.Sleep(5 * time.Second)
	}
}

func (ngm *nvidiaGPUManager) Allocate(ctx context.Context, rqt *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	resp := new(pluginapi.AllocateResponse)
	// Add all requested devices to Allocate Response
	for _, id := range rqt.DevicesIDs {
		dev, ok := ngm.devices[id]
		if !ok {
			return nil, fmt.Errorf("invalid allocation request with non-existing device %s", id)
		}
		if dev.Health != pluginapi.Healthy {
			return nil, fmt.Errorf("invalid allocation request with unhealthy device %s", id)
		}
		resp.Devices = append(resp.Devices, &pluginapi.DeviceSpec{
			HostPath:      "/dev/" + id,
			ContainerPath: "/dev/" + id,
			Permissions:   "mrw",
		})
	}
	// Add all default devices to Allocate Response
	for _, d := range ngm.defaultDevices {
		resp.Devices = append(resp.Devices, &pluginapi.DeviceSpec{
			HostPath:      d,
			ContainerPath: d,
			Permissions:   "mrw",
		})
	}

	resp.Mounts = append(resp.Mounts, &pluginapi.Mount{
		ContainerPath: *containerPathPrefix,
		HostPath:      *hostPathPrefix,
		ReadOnly:      true,
	})
	return resp, nil
}

func (ngm *nvidiaGPUManager) Serve(pMountPath, kEndpoint, pEndpointPrefix string) {
	for {
		pluginEndpoint := fmt.Sprintf("%s-%d.sock", pEndpointPrefix, time.Now().Unix())
		pluginEndpointPath := path.Join(pMountPath, pluginEndpoint)
		glog.Infof("starting device-plugin server at: %s\n", pluginEndpointPath)
		lis, err := net.Listen("unix", pluginEndpointPath)
		if err != nil {
			glog.Fatalf("starting device-plugin server failed: %v", err)
		}
		ngm.grpcServer = grpc.NewServer()
		pluginapi.RegisterDevicePluginServer(ngm.grpcServer, ngm)

		var wg sync.WaitGroup
		wg.Add(1)
		// Starts device plugin service.
		go func() {
			defer wg.Done()
			// Blocking call to accept incoming connections.
			err := ngm.grpcServer.Serve(lis)
			glog.Errorf("device-plugin server stopped serving: %v", err)
		}()

		// Wait till the grpcServer is ready to serve services.
		for len(ngm.grpcServer.GetServiceInfo()) <= 0 {
			time.Sleep(1 * time.Second)
		}
		glog.Infoln("device-plugin server started serving")

		// Registers with Kubelet.
		err = Register(path.Join(pMountPath, kEndpoint), pluginEndpoint, resourceName)
		if err != nil {
			ngm.grpcServer.Stop()
			wg.Wait()
			glog.Fatal(err)
		}
		glog.Infoln("device-plugin registered with the kubelet")

		// This is checking if the plugin socket was deleted. If so,
		// stop the grpc server and start the whole thing again.
		for {
			if _, err := os.Lstat(pluginEndpointPath); err != nil {
				glog.Errorln(err)
				ngm.grpcServer.Stop()
				break
			}
			time.Sleep(1 * time.Second)
		}
		wg.Wait()
	}
}

func main() {
	flag.Parse()
	glog.Infoln("device-plugin started")
	ngm := NewNvidiaGPUManager()
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
	ngm.Serve(pluginMountPath, kubeletEndpoint, pluginEndpointPrefix)
}
