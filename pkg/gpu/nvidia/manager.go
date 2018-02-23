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
	"io/ioutil"
	"net"
	"os"
	"path"
	"regexp"
	"sync"
	"time"

	"github.com/golang/glog"
	"google.golang.org/grpc"

	pluginapi "k8s.io/kubernetes/pkg/kubelet/apis/deviceplugin/v1beta1"
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
)

var (
	resourceName = "nvidia.com/gpu"
)

// nvidiaGPUManager manages nvidia gpu devices.
type nvidiaGPUManager struct {
	hostPathPrefix      string
	containerPathPrefix string
	defaultDevices      []string
	devices             map[string]pluginapi.Device
	grpcServer          *grpc.Server
	socket              string
	stop                chan bool
}

func NewNvidiaGPUManager(hostPathPrefix, containerPathPrefix string) *nvidiaGPUManager {
	return &nvidiaGPUManager{
		hostPathPrefix:      hostPathPrefix,
		containerPathPrefix: containerPathPrefix,
		devices:             make(map[string]pluginapi.Device),
		stop:                make(chan bool),
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
			glog.Infof("Found Nvidia GPU %q\n", f.Name())
			ngm.devices[f.Name()] = pluginapi.Device{ID: f.Name(), Health: pluginapi.Healthy}
		}
	}
	return nil
}

func (ngm *nvidiaGPUManager) GetDeviceState(DeviceName string) string {
	// TODO: calling Nvidia tools to figure out actual device state
	return pluginapi.Healthy
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

func (ngm *nvidiaGPUManager) CheckDeviceStates() bool {
	changed := false
	for id, dev := range ngm.devices {
		state := ngm.GetDeviceState(id)
		if dev.Health != state {
			changed = true
			dev.Health = state
			ngm.devices[id] = dev
		}
	}
	return changed
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
				pluginalpha := &pluginServiceV1Alpha{ngm: ngm}
				pluginalpha.RegisterService()
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
					err = RegisterWithKubelet(path.Join(pMountPath, kEndpoint), pluginEndpoint, resourceName)
					if err != nil {
						glog.Infoln("falling back to v1beta1 API")
						err = RegisterWithV1Beta1Kubelet(path.Join(pMountPath, kEndpoint), pluginEndpoint, resourceName)
					}
					if err != nil {
						ngm.grpcServer.Stop()
						wg.Wait()
						glog.Fatal(err)
					}
					glog.Infoln("device-plugin registered with the kubelet")
				}

				// This is checking if the plugin socket was deleted. If so,
				// stop the grpc server and start the whole thing again.
				for {
					if _, err := os.Lstat(pluginEndpointPath); err != nil {
						glog.Infof("stopping device-plugin server at: %s\n", pluginEndpointPath)
						glog.Errorln(err)
						ngm.grpcServer.Stop()
						break
					}
					time.Sleep(1 * time.Second)
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
	return nil
}
