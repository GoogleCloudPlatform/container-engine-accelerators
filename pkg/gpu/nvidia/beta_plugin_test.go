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

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"golang.org/x/net/context"
	"google.golang.org/grpc"

	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"

	"github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia/timesharing"
)

func TestNvidiaGPUManagerBetaAPI(t *testing.T) {
	testDevDir, err := ioutil.TempDir("", "dev")
	defer os.RemoveAll(testDevDir)

	// Expects a valid GPUManager to be created.
	mountPaths := []MountPath{
		{HostPath: "/home/kubernetes/bin/nvidia", ContainerPath: "/usr/local/nvidia"},
		{HostPath: "/home/kubernetes/bin/vulkan/icd.d", ContainerPath: "/etc/vulkan/icd.d"}}
	testGpuManager := NewNvidiaGPUManager(testDevDir, mountPaths, GPUConfig{})
	as := assert.New(t)
	as.NotNil(testGpuManager)

	testNvidiaCtlDevice := path.Join(testDevDir, nvidiaCtlDevice)
	testNvidiaUVMDevice := path.Join(testDevDir, nvidiaUVMDevice)
	testNvidiaUVMToolsDevice := path.Join(testDevDir, nvidiaUVMToolsDevice)
	testNvidiaModesetDevice := path.Join(testDevDir, nvidiaModesetDevice)
	os.Create(testNvidiaCtlDevice)
	os.Create(testNvidiaUVMDevice)
	os.Create(testNvidiaUVMToolsDevice)
	os.Create(testNvidiaModesetDevice)
	testGpuManager.defaultDevices = []string{testNvidiaCtlDevice, testNvidiaUVMDevice, testNvidiaUVMToolsDevice, testNvidiaModesetDevice}
	defer os.Remove(testNvidiaCtlDevice)
	defer os.Remove(testNvidiaUVMDevice)
	defer os.Remove(testNvidiaUVMToolsDevice)
	defer os.Remove(testNvidiaModesetDevice)

	gpu1 := path.Join(testDevDir, "nvidia1")
	gpu2 := path.Join(testDevDir, "nvidia2")
	os.Create(gpu1)
	os.Create(gpu2)
	defer os.Remove(gpu1)
	defer os.Remove(gpu2)

	// Tests discoverGPUs()
	if _, err := os.Stat(testNvidiaCtlDevice); err == nil {
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

	client := pluginapi.NewDevicePluginClient(conn)

	// Tests ListAndWatch
	stream, err := client.ListAndWatch(context.Background(), &pluginapi.Empty{})
	as.Nil(err)
	devs, err := stream.Recv()
	as.Nil(err)
	devices := make(map[string]*pluginapi.Device)
	for _, d := range devs.Devices {
		devices[d.ID] = d
	}
	as.NotNil(devices["nvidia1"])
	as.NotNil(devices["nvidia2"])

	// Tests Allocate
	resp, err := client.Allocate(context.Background(), &pluginapi.AllocateRequest{
		ContainerRequests: []*pluginapi.ContainerAllocateRequest{
			{DevicesIDs: []string{"nvidia1"}}}})
	as.Nil(err)
	as.Len(resp.ContainerResponses, 1)
	as.Len(resp.ContainerResponses[0].Devices, 5)
	as.Len(resp.ContainerResponses[0].Mounts, 2)
	resp, err = client.Allocate(context.Background(), &pluginapi.AllocateRequest{
		ContainerRequests: []*pluginapi.ContainerAllocateRequest{
			{DevicesIDs: []string{"nvidia1", "nvidia2"}}}})
	as.Nil(err)
	var retDevices []string
	for _, dev := range resp.ContainerResponses[0].Devices {
		retDevices = append(retDevices, dev.HostPath)
	}
	as.Contains(retDevices, gpu1)
	as.Contains(retDevices, gpu2)
	as.Contains(retDevices, testNvidiaCtlDevice)
	as.Contains(retDevices, testNvidiaUVMDevice)
	as.Contains(retDevices, testNvidiaUVMToolsDevice)
	as.Contains(retDevices, testNvidiaModesetDevice)
	resp, err = client.Allocate(context.Background(), &pluginapi.AllocateRequest{
		ContainerRequests: []*pluginapi.ContainerAllocateRequest{
			{DevicesIDs: []string{"nvidia1", "nvidia3"}}}})
	as.Nil(resp)
	as.NotNil(err)

	// Tests detecting new GPUs installed
	gpu3 := path.Join(testDevDir, "nvidia3")
	os.Create(gpu3)
	defer os.Remove(gpu3)
	// The GPU device check is every 10s
	time.Sleep(gpuCheckInterval + 1*time.Second)

	resp, err = client.Allocate(context.Background(), &pluginapi.AllocateRequest{
		ContainerRequests: []*pluginapi.ContainerAllocateRequest{
			{DevicesIDs: []string{"nvidia3"}}}})
	as.Nil(err)

	for _, dev := range resp.ContainerResponses[0].Devices {
		retDevices = append(retDevices, dev.HostPath)
	}
	as.Contains(retDevices, gpu3)
}

func TestNvidiaGPUManagerBetaAPIWithTimeSharingSolution(t *testing.T) {
	wantDevices := map[string]*pluginapi.Device{
		"nvidia0/vgpu0": &pluginapi.Device{
			ID:     "nvidia0/vgpu0",
			Health: pluginapi.Healthy,
		},
		"nvidia0/vgpu1": &pluginapi.Device{
			ID:     "nvidia0/vgpu1",
			Health: pluginapi.Healthy,
		},
		"nvidia1/vgpu0": &pluginapi.Device{
			ID:     "nvidia1/vgpu0",
			Health: pluginapi.Healthy,
		},
		"nvidia1/vgpu1": &pluginapi.Device{
			ID:     "nvidia1/vgpu1",
			Health: pluginapi.Healthy,
		},
	}

	testDevDir, err := ioutil.TempDir("", "dev")
	defer os.RemoveAll(testDevDir)

	// Expects a valid GPUManager to be created.
	mountPaths := []MountPath{
		{HostPath: "/home/kubernetes/bin/nvidia", ContainerPath: "/usr/local/nvidia"},
		{HostPath: "/home/kubernetes/bin/vulkan/icd.d", ContainerPath: "/etc/vulkan/icd.d"}}
	testGpuManager := NewNvidiaGPUManager(testDevDir, mountPaths, GPUConfig{
		GPUSharingConfig: GPUSharingConfig{
			GPUSharingStrategy:     []string{timesharing.TimeSharing},
			MaxSharedClientsPerGPU: 2,
		},
	})
	as := assert.New(t)
	as.NotNil(testGpuManager)

	testNvidiaCtlDevice := path.Join(testDevDir, nvidiaCtlDevice)
	testNvidiaUVMDevice := path.Join(testDevDir, nvidiaUVMDevice)
	testNvidiaUVMToolsDevice := path.Join(testDevDir, nvidiaUVMToolsDevice)
	testNvidiaModesetDevice := path.Join(testDevDir, nvidiaModesetDevice)
	os.Create(testNvidiaCtlDevice)
	os.Create(testNvidiaUVMDevice)
	os.Create(testNvidiaUVMToolsDevice)
	os.Create(testNvidiaModesetDevice)
	testGpuManager.defaultDevices = []string{testNvidiaCtlDevice, testNvidiaUVMDevice, testNvidiaUVMToolsDevice, testNvidiaModesetDevice}
	defer os.Remove(testNvidiaCtlDevice)
	defer os.Remove(testNvidiaUVMDevice)
	defer os.Remove(testNvidiaUVMToolsDevice)
	defer os.Remove(testNvidiaModesetDevice)

	gpu0 := path.Join(testDevDir, "nvidia0")
	gpu1 := path.Join(testDevDir, "nvidia1")
	os.Create(gpu0)
	os.Create(gpu1)
	defer os.Remove(gpu0)
	defer os.Remove(gpu1)

	// Tests discoverGPUs()
	if _, err := os.Stat(testNvidiaCtlDevice); err == nil {
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

	client := pluginapi.NewDevicePluginClient(conn)

	// Tests ListAndWatch.
	stream, err := client.ListAndWatch(context.Background(), &pluginapi.Empty{})
	as.Nil(err)
	devs, err := stream.Recv()
	as.Nil(err)
	devices := make(map[string]*pluginapi.Device)
	for _, d := range devs.Devices {
		devices[d.ID] = d
	}
	if diff := cmp.Diff(wantDevices, devices); diff != "" {
		t.Error("unexpected devices (-want, +got) = ", diff)
	}

	// Tests Allocate.
	// Allocate a valid request.
	resp, err := client.Allocate(context.Background(), &pluginapi.AllocateRequest{
		ContainerRequests: []*pluginapi.ContainerAllocateRequest{
			{DevicesIDs: []string{"nvidia0/vgpu0"}}}})
	as.Nil(err)
	as.Len(resp.ContainerResponses, 1)
	as.Len(resp.ContainerResponses[0].Devices, 5)
	as.Len(resp.ContainerResponses[0].Mounts, 2)
	var retDevices []string
	for _, dev := range resp.ContainerResponses[0].Devices {
		retDevices = append(retDevices, dev.HostPath)
	}
	as.Contains(retDevices, gpu0)
	as.Contains(retDevices, testNvidiaCtlDevice)
	as.Contains(retDevices, testNvidiaUVMDevice)
	as.Contains(retDevices, testNvidiaUVMToolsDevice)
	as.Contains(retDevices, testNvidiaModesetDevice)
	// Allocate an invalid request.
	resp, err = client.Allocate(context.Background(), &pluginapi.AllocateRequest{
		ContainerRequests: []*pluginapi.ContainerAllocateRequest{
			{DevicesIDs: []string{"nvidia0/vgpu0", "nvidia1/vgpu0"}}}})
	as.Nil(resp)
	as.NotNil(err)

	// Tests detecting new GPUs installed.
	gpu2 := path.Join(testDevDir, "nvidia2")
	os.Create(gpu2)
	defer os.Remove(gpu2)
	// The GPU device check is every 10s
	time.Sleep(gpuCheckInterval + 1*time.Second)

	resp, err = client.Allocate(context.Background(), &pluginapi.AllocateRequest{
		ContainerRequests: []*pluginapi.ContainerAllocateRequest{
			{DevicesIDs: []string{"nvidia2/vgpu0"}}}})
	as.Nil(err)
	for _, dev := range resp.ContainerResponses[0].Devices {
		retDevices = append(retDevices, dev.HostPath)
	}
	as.Contains(retDevices, gpu2)
}
