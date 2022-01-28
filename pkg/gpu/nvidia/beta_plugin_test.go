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
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path"
	"reflect"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"golang.org/x/net/context"
	"google.golang.org/grpc"

	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

func TestNvidiaGPUManagerBetaAPI(t *testing.T) {
	cases := []struct {
		name            string
		gpuConfig       GPUConfig
		wantDevices     map[string]*pluginapi.Device
		validRequests   []*pluginapi.ContainerAllocateRequest
		usedRequests    []*pluginapi.ContainerAllocateRequest
		invalidRequests []*pluginapi.ContainerAllocateRequest
		newRequests     []*pluginapi.ContainerAllocateRequest
		mode            string
	}{
		{
			name:      "GPU manager",
			gpuConfig: GPUConfig{},
			wantDevices: map[string]*pluginapi.Device{
				"nvidia0": {
					ID:     "nvidia0",
					Health: pluginapi.Healthy,
				},
				"nvidia1": {
					ID:     "nvidia1",
					Health: pluginapi.Healthy,
				},
			},
			validRequests: []*pluginapi.ContainerAllocateRequest{
				{DevicesIDs: []string{"nvidia0"}},
			},
			usedRequests: []*pluginapi.ContainerAllocateRequest{
				{DevicesIDs: []string{"nvidia0"}},
				{DevicesIDs: []string{"nvidia1"}},
			},
			invalidRequests: []*pluginapi.ContainerAllocateRequest{
				{DevicesIDs: []string{"nvidia1"}},
				{DevicesIDs: []string{"nvidia2"}},
			},
			newRequests: []*pluginapi.ContainerAllocateRequest{
				{DevicesIDs: []string{"nvidia2"}},
			},
		},
		{
			name: "GPU manager with time-sharing",
			gpuConfig: GPUConfig{
				GPUSharingConfig: GPUSharingConfig{
					GPUSharingStrategy:     "time-sharing",
					MaxSharedClientsPerGPU: 2,
				},
			},
			wantDevices: map[string]*pluginapi.Device{
				"nvidia0/vgpu0": {
					ID:     "nvidia0/vgpu0",
					Health: pluginapi.Healthy,
				},
				"nvidia0/vgpu1": {
					ID:     "nvidia0/vgpu1",
					Health: pluginapi.Healthy,
				},
				"nvidia1/vgpu0": {
					ID:     "nvidia1/vgpu0",
					Health: pluginapi.Healthy,
				},
				"nvidia1/vgpu1": {
					ID:     "nvidia1/vgpu1",
					Health: pluginapi.Healthy,
				},
			},
			validRequests: []*pluginapi.ContainerAllocateRequest{
				{DevicesIDs: []string{"nvidia0/vgpu0"}},
			},
			usedRequests: []*pluginapi.ContainerAllocateRequest{
				{DevicesIDs: []string{"nvidia0/vgpu0"}},
				{DevicesIDs: []string{"nvidia1/vgpu0"}},
			},
			invalidRequests: []*pluginapi.ContainerAllocateRequest{
				{DevicesIDs: []string{"nvidia1/vgpu0"}},
				{DevicesIDs: []string{"nvidia2/vgpu0"}},
			},
			newRequests: []*pluginapi.ContainerAllocateRequest{
				{DevicesIDs: []string{"nvidia2/vgpu0"}},
			},
		},
		{
			name: "GPU manager for MIG",
			gpuConfig: GPUConfig{
				GPUPartitionSize: "3g.20gb",
			},
			wantDevices: map[string]*pluginapi.Device{
				"nvidia0/gi1": {
					ID:     "nvidia0/gi1",
					Health: pluginapi.Healthy,
				},
				"nvidia0/gi2": {
					ID:     "nvidia0/gi2",
					Health: pluginapi.Healthy,
				},
			},
			validRequests: []*pluginapi.ContainerAllocateRequest{
				{DevicesIDs: []string{"nvidia0/gi1"}},
			},
			usedRequests: []*pluginapi.ContainerAllocateRequest{
				{DevicesIDs: []string{"nvidia0/gi1"}},
				{DevicesIDs: []string{"nvidia0/gi2"}},
			},
			invalidRequests: []*pluginapi.ContainerAllocateRequest{
				{DevicesIDs: []string{"nvidia0/gi2"}},
				{DevicesIDs: []string{"nvidia1/gi1"}},
			},
			newRequests: []*pluginapi.ContainerAllocateRequest{
				{DevicesIDs: []string{"nvidia1/gi1"}},
			},
			mode: "MIG",
		},
		{
			name: "GPU manager for MIG with time-sharing",
			gpuConfig: GPUConfig{
				GPUSharingConfig: GPUSharingConfig{
					GPUSharingStrategy:     "time-sharing",
					MaxSharedClientsPerGPU: 2,
				},
				GPUPartitionSize: "3g.20gb",
			},
			wantDevices: map[string]*pluginapi.Device{
				"nvidia0/gi1/vgpu0": {
					ID:     "nvidia0/gi1/vgpu0",
					Health: pluginapi.Healthy,
				},
				"nvidia0/gi2/vgpu0": {
					ID:     "nvidia0/gi2/vgpu0",
					Health: pluginapi.Healthy,
				},
				"nvidia0/gi1/vgpu1": {
					ID:     "nvidia0/gi1/vgpu1",
					Health: pluginapi.Healthy,
				},
				"nvidia0/gi2/vgpu1": {
					ID:     "nvidia0/gi2/vgpu1",
					Health: pluginapi.Healthy,
				},
			},
			validRequests: []*pluginapi.ContainerAllocateRequest{
				{DevicesIDs: []string{"nvidia0/gi1/vgpu1"}},
			},
			usedRequests: []*pluginapi.ContainerAllocateRequest{
				{DevicesIDs: []string{"nvidia0/gi1/vgpu1"}},
				{DevicesIDs: []string{"nvidia0/gi2/vgpu1"}},
			},
			invalidRequests: []*pluginapi.ContainerAllocateRequest{
				{DevicesIDs: []string{"nvidia0/gi2/vgpu1"}},
				{DevicesIDs: []string{"nvidia1/gi1/vgpu1"}},
			},
			newRequests: []*pluginapi.ContainerAllocateRequest{
				{DevicesIDs: []string{"nvidia1/gi1/vgpu1"}},
			},
			mode: "MIG",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			if tc.mode == "MIG" {
				err = testNvidiaGPUManagerBetaAPIWithMig(tc.gpuConfig, tc.wantDevices, tc.validRequests, tc.usedRequests, tc.invalidRequests, tc.newRequests)
			} else {
				err = testNvidiaGPUManagerBetaAPI(tc.gpuConfig, tc.wantDevices, tc.validRequests, tc.usedRequests, tc.invalidRequests, tc.newRequests)
			}
			if err != nil {
				t.Error("unexpected error: ", err)
			}
		})
	}

}

func testNvidiaGPUManagerBetaAPI(gpuConfig GPUConfig, wantDevices map[string]*pluginapi.Device, validRequests []*pluginapi.ContainerAllocateRequest, usedRequests []*pluginapi.ContainerAllocateRequest, invalidRequests []*pluginapi.ContainerAllocateRequest, newRequests []*pluginapi.ContainerAllocateRequest) error {
	testDevDir, err := ioutil.TempDir("", "dev")
	defer os.RemoveAll(testDevDir)

	// Create device nodes
	deviceNodes := []string{
		nvidiaCtlDevice,
		nvidiaUVMDevice,
		nvidiaUVMToolsDevice,
		nvidiaModesetDevice,
		"nvidia0",
		"nvidia1",
	}
	for _, device := range deviceNodes {
		os.Create(path.Join(testDevDir, device))
	}
	defer func() {
		for _, device := range deviceNodes {
			os.Remove(path.Join(testDevDir, device))
		}
	}()

	// Expects a valid GPUManager to be created.
	mountPaths := []pluginapi.Mount{
		{HostPath: "/home/kubernetes/bin/nvidia", ContainerPath: "/usr/local/nvidia", ReadOnly: true},
		{HostPath: "/home/kubernetes/bin/vulkan/icd.d", ContainerPath: "/etc/vulkan/icd.d", ReadOnly: true}}
	testGpuManager := NewNvidiaGPUManager(testDevDir, "", mountPaths, gpuConfig)
	if testGpuManager == nil {
		return fmt.Errorf("failed to initilize a GPU manager")
	}

	// Start GPU manager.
	if err := testGpuManager.Start(); err != nil {
		return fmt.Errorf("unable to start gpu manager: %w", err)
	}

	// Tests discoverGPUs()
	os.Stat(path.Join(testDevDir, nvidiaCtlDevice))
	discoverErr := testGpuManager.discoverGPUs()
	if discoverErr != nil {
		return discoverErr
	}
	gpus := reflect.ValueOf(testGpuManager).Elem().FieldByName("devices").Len()
	if gpus == 0 {
		return fmt.Errorf("unable to discover GPU devices")
	}

	testdir, err := ioutil.TempDir("", "gpu_device_plugin")
	if err != nil {
		return fmt.Errorf("error for creating temp dir gpu_device_plugin: %w", err)
	}
	defer os.RemoveAll(testdir)

	kubeletEndpoint := path.Join(testdir, "kubelet.sock")
	kubeletStub := NewKubeletStub(kubeletEndpoint)
	kubeletStub.Start()
	defer kubeletStub.server.Stop()

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
	if err != nil {
		return fmt.Errorf("error for creating grpc connection: %w", err)
	}
	defer conn.Close()

	client := pluginapi.NewDevicePluginClient(conn)

	// Tests ListAndWatch
	stream, err := client.ListAndWatch(context.Background(), &pluginapi.Empty{})
	if err != nil {
		return fmt.Errorf("error for making list and watch action: %w", err)
	}
	devs, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("error for recieving stream: %w", err)
	}
	devices := make(map[string]*pluginapi.Device)
	for _, d := range devs.Devices {
		devices[d.ID] = d
	}
	if diff := cmp.Diff(wantDevices, devices); diff != "" {
		return fmt.Errorf("unexpected devices (-want, +got) = %s", diff)
	}

	// Tests Allocate
	resp, err := client.Allocate(context.Background(), &pluginapi.AllocateRequest{
		ContainerRequests: validRequests})

	if err != nil {
		return fmt.Errorf("error for allocating a valid request: %w", err)
	}
	if diff := cmp.Diff(5, len(resp.ContainerResponses[0].Devices)); diff != "" {
		return fmt.Errorf("unexpected devices in resp (-want, +got) = %s", diff)
	}
	if diff := cmp.Diff(2, len(resp.ContainerResponses[0].Mounts)); diff != "" {
		return fmt.Errorf("unexpected mounts in resp (-want, +got) = %s", diff)
	}
	resp, err = client.Allocate(context.Background(), &pluginapi.AllocateRequest{
		ContainerRequests: usedRequests})
	if err != nil {
		return fmt.Errorf("error for allocating a duplicated request: %w", err)
	}

	resp, err = client.Allocate(context.Background(), &pluginapi.AllocateRequest{
		ContainerRequests: invalidRequests})
	if resp != nil {
		return fmt.Errorf("non-nil resp when allocating an invalid request: %v", resp)
	}
	if err == nil {
		return fmt.Errorf("nil err when allocating an invalid request")
	}

	// Tests detecting new GPU
	gpu2 := path.Join(testDevDir, "nvidia2")
	os.Create(gpu2)
	defer os.Remove(gpu2)
	// The GPU device check is every 10s
	time.Sleep(gpuCheckInterval + 1*time.Second)

	resp, err = client.Allocate(context.Background(), &pluginapi.AllocateRequest{
		ContainerRequests: newRequests})
	if err != nil {
		return fmt.Errorf("error for allocating a request after adding a new GPU device: %w", err)
	}

	return nil
}

func testNvidiaGPUManagerBetaAPIWithMig(gpuConfig GPUConfig, wantDevices map[string]*pluginapi.Device, validRequests []*pluginapi.ContainerAllocateRequest, usedRequests []*pluginapi.ContainerAllocateRequest, invalidRequests []*pluginapi.ContainerAllocateRequest, newRequests []*pluginapi.ContainerAllocateRequest) error {
	testDevDir, err := ioutil.TempDir("", "dev")
	defer os.RemoveAll(testDevDir)
	testProcDir, err := ioutil.TempDir("", "proc")
	defer os.RemoveAll(testProcDir)

	paths := []string{
		"driver/nvidia/capabilities/gpu0/mig/gi1/ci0",
		"driver/nvidia/capabilities/gpu0/mig/gi2/ci0",
	}
	for _, p := range paths {
		if err := os.MkdirAll(path.Join(testProcDir, p), 0755); err != nil {
			return fmt.Errorf("failed to make dir: %w", err)
		}
	}
	defer func() {
		for _, p := range paths {
			os.RemoveAll(path.Join(testProcDir, p))
		}
	}()

	if err := os.MkdirAll(path.Join(testDevDir, "nvidia-caps"), 0755); err != nil {
		return fmt.Errorf("failed to make dir: %w", err)
	}
	defer os.RemoveAll(path.Join(testDevDir, "nvidia-caps"))

	// Create device nodes
	deviceNodes := []string{
		nvidiaCtlDevice,
		nvidiaUVMDevice,
		nvidiaUVMToolsDevice,
		nvidiaModesetDevice,
		"nvidia0",
		"nvidia-caps/nvidia-cap12",
		"nvidia-caps/nvidia-cap13",
		"nvidia-caps/nvidia-cap21",
		"nvidia-caps/nvidia-cap22",
	}
	for _, device := range deviceNodes {
		os.Create(path.Join(testDevDir, device))
	}
	defer func() {
		for _, device := range deviceNodes {
			os.Remove(path.Join(testDevDir, device))
		}
	}()

	capToMinorDevices := map[string]int{
		"driver/nvidia/capabilities/gpu0/mig/gi1/access":     12,
		"driver/nvidia/capabilities/gpu0/mig/gi1/ci0/access": 13,
		"driver/nvidia/capabilities/gpu0/mig/gi2/access":     21,
		"driver/nvidia/capabilities/gpu0/mig/gi2/ci0/access": 22,
	}
	for file, minor := range capToMinorDevices {
		if err := ioutil.WriteFile(path.Join(testProcDir, file), []byte(fmt.Sprintf("DeviceFileMinor: %d\nDeviceFileMode: 292", minor)), 0644); err != nil {
			return fmt.Errorf("failed to create proc capabilities file (%s): %v", file, err)
		}
	}

	// Expects a valid GPUManager to be created.
	mountPaths := []pluginapi.Mount{
		{HostPath: "/home/kubernetes/bin/nvidia", ContainerPath: "/usr/local/nvidia", ReadOnly: true},
		{HostPath: "/home/kubernetes/bin/vulkan/icd.d", ContainerPath: "/etc/vulkan/icd.d", ReadOnly: true}}
	testGpuManager := NewNvidiaGPUManager(testDevDir, testProcDir, mountPaths, gpuConfig)
	if testGpuManager == nil {
		return fmt.Errorf("failed to initilize a GPU manager")
	}

	// Start GPU manager.
	if err := testGpuManager.Start(); err != nil {
		return fmt.Errorf("unable to start gpu manager: %w", err)
	}

	// Tests discoverGPUs()
	discoverErr := testGpuManager.discoverGPUs()
	if discoverErr != nil {
		return discoverErr
	}
	gpus := reflect.ValueOf(testGpuManager).Elem().FieldByName("devices").Len()
	if gpus == 0 {
		return fmt.Errorf("unable to discover GPU devices")
	}

	testdir, err := ioutil.TempDir("", "gpu_device_plugin")
	if err != nil {
		return fmt.Errorf("error for creating temp dir gpu_device_plugin: %w", err)
	}
	defer os.RemoveAll(testdir)

	kubeletEndpoint := path.Join(testdir, "kubelet.sock")
	kubeletStub := NewKubeletStub(kubeletEndpoint)
	kubeletStub.Start()
	defer kubeletStub.server.Stop()

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
	if err != nil {
		return fmt.Errorf("error for creating grpc connection: %w", err)
	}
	defer conn.Close()

	client := pluginapi.NewDevicePluginClient(conn)

	// Tests ListAndWatch
	stream, err := client.ListAndWatch(context.Background(), &pluginapi.Empty{})
	if err != nil {
		return fmt.Errorf("error for making list and watch action: %w", err)
	}
	devs, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("error for recieving stream: %w", err)
	}
	devices := make(map[string]*pluginapi.Device)
	for _, d := range devs.Devices {
		devices[d.ID] = d
	}
	if diff := cmp.Diff(wantDevices, devices); diff != "" {
		return fmt.Errorf("unexpected devices (-want, +got) = %s", diff)
	}

	// Tests Allocate
	resp, err := client.Allocate(context.Background(), &pluginapi.AllocateRequest{
		ContainerRequests: validRequests,
	})
	if err != nil {
		return fmt.Errorf("error for allocating a valid request: %w", err)
	}
	if diff := cmp.Diff(7, len(resp.ContainerResponses[0].Devices)); diff != "" {
		return fmt.Errorf("unexpected devices in resp (-want, +got) = %s", diff)
	}
	if diff := cmp.Diff(2, len(resp.ContainerResponses[0].Mounts)); diff != "" {
		return fmt.Errorf("unexpected mounts in resp (-want, +got) = %s", diff)
	}
	resp, err = client.Allocate(context.Background(), &pluginapi.AllocateRequest{
		ContainerRequests: usedRequests,
	})
	if err != nil {
		return fmt.Errorf("error for allocating a duplicated request: %w", err)
	}

	resp, err = client.Allocate(context.Background(), &pluginapi.AllocateRequest{
		ContainerRequests: invalidRequests,
	})
	if resp != nil {
		return fmt.Errorf("non-nil resp when allocating an invalid request: %v", resp)
	}
	if err == nil {
		return fmt.Errorf("nil err when allocating an invalid request")
	}

	// Tests detecting new GPU
	newPaths := []string{
		"driver/nvidia/capabilities/gpu1/mig/gi1/ci0",
		"driver/nvidia/capabilities/gpu1/mig/gi2/ci0",
	}
	for _, p := range newPaths {
		if err := os.MkdirAll(path.Join(testProcDir, p), 0755); err != nil {
			return fmt.Errorf("failed to make dir: %w", err)
		}
	}
	defer func() {
		for _, p := range paths {
			os.RemoveAll(path.Join(testProcDir, p))
		}
	}()

	newDeviceNodes := []string{
		"nvidia1",
		"nvidia-caps/nvidia-cap11",
		"nvidia-caps/nvidia-cap10",
		"nvidia-caps/nvidia-cap23",
		"nvidia-caps/nvidia-cap14",
	}
	for _, device := range newDeviceNodes {
		os.Create(path.Join(testDevDir, device))
	}
	defer func() {
		for _, device := range deviceNodes {
			os.Remove(path.Join(testDevDir, device))
		}
	}()

	newCapToMinorDevices := map[string]int{
		"driver/nvidia/capabilities/gpu1/mig/gi1/access":     11,
		"driver/nvidia/capabilities/gpu1/mig/gi1/ci0/access": 10,
		"driver/nvidia/capabilities/gpu1/mig/gi2/access":     23,
		"driver/nvidia/capabilities/gpu1/mig/gi2/ci0/access": 14,
	}
	for file, minor := range newCapToMinorDevices {
		if err := ioutil.WriteFile(path.Join(testProcDir, file), []byte(fmt.Sprintf("DeviceFileMinor: %d\nDeviceFileMode: 292", minor)), 0644); err != nil {
			return fmt.Errorf("failed to create proc capabilities file (%s): %v", file, err)
		}
	}
	testGpuManager.Start()

	// The GPU device check is every 10s
	time.Sleep(gpuCheckInterval + 1*time.Second)

	resp, err = client.Allocate(context.Background(), &pluginapi.AllocateRequest{
		ContainerRequests: newRequests,
	})
	if err != nil {
		return fmt.Errorf("error for allocating a request after adding a new GPU device: %w", err)
	}

	return nil
}
