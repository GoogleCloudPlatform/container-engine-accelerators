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
	"io/ioutil"
	"net"
	"os"
	"path"
	"regexp"
	"sync"
	"time"

	"github.com/golang/glog"
	"golang.org/x/net/context"
	"google.golang.org/grpc"

	pluginapi "k8s.io/kubernetes/pkg/kubelet/apis/deviceplugin/v1alpha1"
)

const (
	// All NVIDIA GPUs cards should be mounted with nvidiactl and nvidia-uvm
	// If the driver installed correctly, the 2 devices will be there.
	nvidiaCtlDevice string = "/dev/nvidiactl"
	nvidiaUVMDevice string = "/dev/nvidia-uvm"
	// Optional device.
	nvidiaUVMToolsDevice string = "/dev/nvidia-uvm-tools"
	devDirectory                = "/dev"
	nvidiaDeviceRE              = `^nvidia[0-9]*$`

	// Device plugin settings.
	devicePluginMountPath = "/device-plugin"
	kubeletEndpoint       = "kubelet.sock"
	pluginEndpointPrefix  = "nvidiaGPU"
	resourceName          = "nvidia.com/gpu"
	ContainerPathPrefix   = "/usr/local/nvidia"
	HostPathPrefix        = "/home/kubernetes/bin/nvidia"
)

// nvidiaGPUManager manages nvidia gpu devices.
type nvidiaGPUManager struct {
	sync.Mutex
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
	reg := regexp.MustCompile(nvidiaDeviceRE)
	files, err := ioutil.ReadDir(devDirectory)
	if err != nil {
		return err
	}
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		if reg.MatchString(f.Name()) {
			fmt.Printf("Found Nvidia GPU %q\n", f.Name())
			ngm.devices[f.Name()] = pluginapi.Device{f.Name(), pluginapi.Healthy}
		}
	}

	return nil
}

func (ngm *nvidiaGPUManager) GetDeviceState(DeviceName string) string {
	// TODO: calling Nvidia tools to figure out actual device state
	return pluginapi.Healthy
}

// Discovers Nvidia GPU devices, installs device drivers, and sets up device
// access environment.
func (ngm *nvidiaGPUManager) Start() error {
	if _, err := os.Stat(nvidiaCtlDevice); err != nil {
		return err
	}

	if _, err := os.Stat(nvidiaUVMDevice); err != nil {
		return err
	}

	ngm.defaultDevices = []string{nvidiaCtlDevice, nvidiaUVMDevice}

	if _, err := os.Stat(nvidiaUVMToolsDevice); !os.IsNotExist(err) {
		ngm.defaultDevices = append(ngm.defaultDevices, nvidiaUVMToolsDevice)
	}

	if err := ngm.discoverGPUs(); err != nil {
		return err
	}

	return nil
}

func Register(kubeletEndpoint, pluginEndpoint, resourceName string) error {
	conn, err := grpc.Dial(kubeletEndpoint, grpc.WithInsecure(),
		grpc.WithDialer(func(addr string, timeout time.Duration) (net.Conn, error) {
			return net.DialTimeout("unix", addr, timeout)
		}))
	defer conn.Close()
	if err != nil {
		return fmt.Errorf("device-plugin: cannot connect to kubelet service: %v", err)
	}
	client := pluginapi.NewRegistrationClient(conn)
	reqt := &pluginapi.RegisterRequest{
		Version:      pluginapi.Version,
		Endpoint:     pluginEndpoint,
		ResourceName: resourceName,
	}

	_, err = client.Register(context.Background(), reqt)
	if err != nil {
		return fmt.Errorf("device-plugin: cannot register to kubelet service: %v", err)
	}
	return nil
}

// Implements DevicePlugin service functions
func (ngm *nvidiaGPUManager) ListAndWatch(emtpy *pluginapi.Empty, stream pluginapi.DevicePlugin_ListAndWatchServer) error {
	fmt.Printf("device-plugin: ListAndWatch start\n")
	changed := true
	for {
		for id, dev := range ngm.devices {
			state := ngm.GetDeviceState(id)
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
			fmt.Printf("ListAndWatch: send devices %v\n", resp)
			if err := stream.Send(resp); err != nil {
				fmt.Printf("device-plugin: cannot update device states: %v\n", err)
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
	for _, id := range rqt.DevicesIDs {
		dev, ok := ngm.devices[id]
		if !ok {
			return nil, fmt.Errorf("Invalid allocation request with non-existing device %s", id)
		}
		if dev.Health != pluginapi.Healthy {
			return nil, fmt.Errorf("Invalid allocation request with unhealthy device %s", id)
		}
		devRuntime := new(pluginapi.DeviceRuntimeSpec)
		devRuntime.Devices = append(devRuntime.Devices, &pluginapi.DeviceSpec{
			HostPath:      "/dev/" + id,
			ContainerPath: "/dev/" + id,
			Permissions:   "mrw",
		})
		for _, d := range ngm.defaultDevices {
			devRuntime.Devices = append(devRuntime.Devices, &pluginapi.DeviceSpec{
				HostPath:      d,
				ContainerPath: d,
				Permissions:   "mrw",
			})
		}
		devRuntime.Mounts = append(devRuntime.Mounts, &pluginapi.Mount{
			ContainerPath: path.Join(ContainerPathPrefix, "lib64"),
			HostPath:      path.Join(HostPathPrefix, "lib"),
			ReadOnly:      true,
		})
		devRuntime.Mounts = append(devRuntime.Mounts, &pluginapi.Mount{
			ContainerPath: path.Join(ContainerPathPrefix, "bin"),
			HostPath:      path.Join(HostPathPrefix, "bin"),
			ReadOnly:      true,
		})
		resp.Spec = append(resp.Spec, devRuntime)
	}
	return resp, nil
}

func (ngm *nvidiaGPUManager) Serve(dpMountPath, kEndpoint, pEndpointPrefix string) {
	for {
		pluginEndpoint := fmt.Sprintf("%s-%d.sock", pEndpointPrefix, time.Now().Unix())
		pluginEndpointPath := path.Join(dpMountPath, pluginEndpoint)
		var wg sync.WaitGroup
		wg.Add(1)
		// Starts device plugin service.
		go func() {
			defer wg.Done()
			fmt.Printf("device-plugin start server at: %s\n", pluginEndpointPath)
			lis, err := net.Listen("unix", pluginEndpointPath)
			if err != nil {
				glog.Fatal(err)
				return
			}
			grpcServer := grpc.NewServer()
			pluginapi.RegisterDevicePluginServer(grpcServer, ngm)
			ngm.Lock()
			ngm.grpcServer = grpcServer
			ngm.Unlock()
			ngm.grpcServer.Serve(lis)
		}()

		// Wait till the grpcServer is ready to serve services.
		for {
			ngm.Lock()
			server := ngm.grpcServer
			ngm.Unlock()
			if server != nil {
				services := server.GetServiceInfo()
				if len(services) > 0 {
					break
				}
			}
			time.Sleep(1 * time.Second)
		}

		// Registers with Kubelet.
		err := Register(path.Join(dpMountPath, kEndpoint), pluginEndpoint, resourceName)
		if err != nil {
			glog.Fatal(err)
			os.Exit(1)
		}
		fmt.Printf("device-plugin registered\n")

		for {
			if _, err := os.Lstat(pluginEndpointPath); err != nil {
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
	fmt.Printf("device-plugin started\n")
	ngm := NewNvidiaGPUManager()
	for {
		err := ngm.Start()
		if err == nil {
			break
		}
		time.Sleep(5 * time.Second)
	}
	ngm.Serve(devicePluginMountPath, kubeletEndpoint, pluginEndpointPrefix)
}
