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
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"golang.org/x/net/context"
	"google.golang.org/grpc"

	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1alpha"
	betapluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

type KubeletStub struct {
	sync.Mutex
	socket         string
	pluginEndpoint string
	server         *grpc.Server
}

// NewKubeletStub returns an initialized KubeletStub for testing purpose.
func NewKubeletStub(socket string) *KubeletStub {
	return &KubeletStub{
		socket: socket,
	}
}

func (k *KubeletStub) Register(ctx context.Context, r *pluginapi.RegisterRequest) (*pluginapi.Empty, error) {
	k.Lock()
	defer k.Unlock()
	k.pluginEndpoint = r.Endpoint
	return &pluginapi.Empty{}, nil
}

func (k *KubeletStub) Start() error {
	os.Remove(k.socket)
	s, err := net.Listen("unix", k.socket)
	if err != nil {
		fmt.Printf("Can't listen at the socket: %+v", err)
		return err
	}

	k.server = grpc.NewServer([]grpc.ServerOption{}...)

	pluginapi.RegisterRegistrationServer(k.server, k)
	go k.server.Serve(s)
	return nil
}

func TestRegister(t *testing.T) {
	testdir, err := ioutil.TempDir("", "gpu_device_plugin")
	as := assert.New(t)
	as.Nil(err)
	defer os.RemoveAll(testdir)
	kubeletEndpoint := path.Join(testdir, "kubelet.sock")
	pluginEndpoint := path.Join(testdir, "plugin.sock")
	resourceName := "nvidia.com/GPU"
	kubeletStub := NewKubeletStub(kubeletEndpoint)
	kubeletStub.Start()
	defer kubeletStub.server.Stop()
	err = RegisterWithKubelet(kubeletEndpoint, pluginEndpoint, resourceName)
	as.Nil(err)
}

func TestNvidiaGPUManagerAlphaAPI(t *testing.T) {
	cases := []struct {
		name             string
		gpuConfig        GPUConfig
		wantDevices      map[string]*pluginapi.Device
		validDeviceIDs   []string
		usedDeviceIDs    []string
		invalidDeviceIDs []string
		newDeviceIDs     []string
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
			validDeviceIDs:   []string{"nvidia0"},
			usedDeviceIDs:    []string{"nvidia0", "nvidia1"},
			invalidDeviceIDs: []string{"nvidia1", "nvidia2"},
			newDeviceIDs:     []string{"nvidia2"},
		},
		{
			name: "GPU manager with time-sharing",
			gpuConfig: GPUConfig{
				MaxTimeSharedClientsPerGPU: 2,
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
			validDeviceIDs:   []string{"nvidia0/vgpu0"},
			usedDeviceIDs:    []string{"nvidia0/vgpu0"},
			invalidDeviceIDs: []string{"nvidia2/vgpu0"},
			newDeviceIDs:     []string{"nvidia2/vgpu0"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// We don't include UTs in MIG mode as alpha plugin doesn't support MIG.
			err := testNvidiaGPUManagerAlphaAPI(tc.gpuConfig, tc.wantDevices, tc.validDeviceIDs, tc.usedDeviceIDs, tc.invalidDeviceIDs, tc.newDeviceIDs)
			if err != nil {
				t.Error("unexpected error: ", err)
			}
		})
	}
}

func testNvidiaGPUManagerAlphaAPI(gpuConfig GPUConfig, wantDevices map[string]*pluginapi.Device, validDeviceIDs []string, usedDeviceIDs []string, invalidDeviceIDs []string, newDeviceIDs []string) error {
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
	mountPaths := []betapluginapi.Mount{
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
		DevicesIDs: validDeviceIDs,
	})
	if err != nil {
		return fmt.Errorf("error for allocating a valid request: %w", err)
	}
	if diff := cmp.Diff(5, len(resp.Devices)); diff != "" {
		return fmt.Errorf("unexpected devices in resp (-want, +got) = %s", diff)
	}
	if diff := cmp.Diff(2, len(resp.Mounts)); diff != "" {
		return fmt.Errorf("unexpected mounts in resp (-want, +got) = %s", diff)
	}
	resp, err = client.Allocate(context.Background(), &pluginapi.AllocateRequest{
		DevicesIDs: usedDeviceIDs,
	})
	if err != nil {
		return fmt.Errorf("error for allocating a duplicated request: %w", err)
	}

	resp, err = client.Allocate(context.Background(), &pluginapi.AllocateRequest{
		DevicesIDs: invalidDeviceIDs,
	})
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
		DevicesIDs: newDeviceIDs,
	})
	if err != nil {
		return fmt.Errorf("error for allocating a request after adding a new GPU device: %w", err)
	}

	return nil
}
