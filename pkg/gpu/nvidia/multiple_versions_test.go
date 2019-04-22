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
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"golang.org/x/net/context"
	"google.golang.org/grpc"

	pluginalpha "k8s.io/kubernetes/pkg/kubelet/apis/deviceplugin/v1alpha"
	pluginbeta "k8s.io/kubernetes/pkg/kubelet/apis/deviceplugin/v1beta1"
)

func TestNvidiaGPUManagerMultuipleAPIs(t *testing.T) {
	// Expects a valid GPUManager to be created.
	testGpuManager := NewNvidiaGPUManager("/home/kubernetes/bin/nvidia", "/usr/local/nvidia")
	as := assert.New(t)
	as.NotNil(testGpuManager)

	testGpuManager.defaultDevices = []string{nvidiaCtlDevice, nvidiaUVMDevice, nvidiaUVMToolsDevice}
	// Tests discoverGPUs()
	if _, err := os.Stat(nvidiaCtlDevice); err == nil {
		err = testGpuManager.discoverGPUs()
		as.Nil(err)
		gpus := reflect.ValueOf(testGpuManager).Elem().FieldByName("devices").Len()
		as.NotZero(gpus)
	}

	testdir, err := ioutil.TempDir("", "gpu_device_plugin")
	as.Nil(err)
	defer os.RemoveAll(testdir)

	go func() {
		testGpuManager.Serve(testdir, "kubelet.sock", "plugin.sock")
	}()

	time.Sleep(5 * time.Second)
	devicePluginSock := path.Join(testdir, "plugin.sock")
	defer testGpuManager.Stop()
	// Verifies the grpcServer is ready to serve services.
	conn, err := grpc.Dial(devicePluginSock, grpc.WithInsecure(), grpc.WithBlock(),
		grpc.WithTimeout(10*time.Second),
		grpc.WithDialer(func(addr string, timeout time.Duration) (net.Conn, error) {
			return net.DialTimeout("unix", addr, timeout)
		}))
	as.Nil(err)
	defer conn.Close()

	clientAlpha := pluginalpha.NewDevicePluginClient(conn)
	clientBeta := pluginbeta.NewDevicePluginClient(conn)

	// Tests Beta ListAndWatch
	testGpuManager.devices["dev1"] = pluginbeta.Device{ID: "dev1", Health: pluginbeta.Healthy}
	testGpuManager.devices["dev2"] = pluginbeta.Device{ID: "dev2", Health: pluginbeta.Healthy}
	stream, err := clientBeta.ListAndWatch(context.Background(), &pluginbeta.Empty{})
	as.Nil(err)
	devs, err := stream.Recv()
	as.Nil(err)
	devices := make(map[string]*pluginbeta.Device)
	for _, d := range devs.Devices {
		devices[d.ID] = d
	}
	as.NotNil(devices["dev1"])
	as.NotNil(devices["dev2"])

	// Tests Beta Allocate
	resp, err := clientBeta.Allocate(context.Background(), &pluginbeta.AllocateRequest{
		ContainerRequests: []*pluginbeta.ContainerAllocateRequest{
			{DevicesIDs: []string{"dev1"}}}})
	as.Nil(err)
	as.Len(resp.ContainerResponses, 1)
	as.Len(resp.ContainerResponses[0].Devices, 4)
	as.Len(resp.ContainerResponses[0].Mounts, 1)
	resp, err = clientBeta.Allocate(context.Background(), &pluginbeta.AllocateRequest{
		ContainerRequests: []*pluginbeta.ContainerAllocateRequest{
			{DevicesIDs: []string{"dev1", "dev2"}}}})
	as.Nil(err)
	var retDevices []string
	for _, dev := range resp.ContainerResponses[0].Devices {
		retDevices = append(retDevices, dev.HostPath)
	}
	as.Contains(retDevices, "/dev/dev1")
	as.Contains(retDevices, "/dev/dev2")
	as.Contains(retDevices, "/dev/nvidiactl")
	as.Contains(retDevices, "/dev/nvidia-uvm")
	as.Contains(retDevices, "/dev/nvidia-uvm-tools")
	resp, err = clientBeta.Allocate(context.Background(), &pluginbeta.AllocateRequest{
		ContainerRequests: []*pluginbeta.ContainerAllocateRequest{
			{DevicesIDs: []string{"dev1", "dev3"}}}})
	as.Nil(resp)
	as.NotNil(err)

	// Tests Alpha ListAndWatch
	testGpuManager.devices["dev1"] = pluginbeta.Device{ID: "dev1", Health: pluginalpha.Healthy}
	testGpuManager.devices["dev2"] = pluginbeta.Device{ID: "dev2", Health: pluginalpha.Healthy}
	stream2, err := clientAlpha.ListAndWatch(context.Background(), &pluginalpha.Empty{})
	as.Nil(err)
	devs2, err := stream2.Recv()
	as.Nil(err)
	devices2 := make(map[string]*pluginalpha.Device)
	for _, d := range devs2.Devices {
		devices2[d.ID] = d
	}
	as.NotNil(devices2["dev1"])
	as.NotNil(devices2["dev2"])
}
