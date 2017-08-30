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
	"fmt"
	"net"
	"os"
	"path"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"golang.org/x/net/context"
	"google.golang.org/grpc"

	pluginapi "k8s.io/kubernetes/pkg/kubelet/apis/deviceplugin/v1alpha1"
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
	kubeletEndpoint := "/tmp/kubelet.sock"
	pluginEndpoint := "/tmp/plugin.sock"
	resourceName := "nvidia.com/GPU"
	kubeletStub := NewKubeletStub(kubeletEndpoint)
	kubeletStub.Start()
	defer kubeletStub.server.Stop()
	err := Register(kubeletEndpoint, pluginEndpoint, resourceName)
	as := assert.New(t)
	as.Nil(err)
}

func TestNvidiaGPUManager(t *testing.T) {
	// Expects a valid GPUManager to be created.
	testGpuManager, err := NewNvidiaGPUManager()
	as := assert.New(t)
	as.NotNil(testGpuManager)
	as.Nil(err)

	// Tests discoverGPUs()
	if _, err = os.Stat(nvidiaCtlDevice); err == nil {
		err = testGpuManager.discoverGPUs()
		as.Nil(err)
		gpus := reflect.ValueOf(testGpuManager).Elem().FieldByName("devices").Len()
		as.NotZero(gpus)
	}

	kubeletEndpoint := "/tmp/kubelet.sock"
	kubeletStub := NewKubeletStub(kubeletEndpoint)
	kubeletStub.Start()
	defer kubeletStub.server.Stop()

	go func() {
		testGpuManager.Serve("/tmp", "kubelet.sock", "plugin.sock")
	}()

	time.Sleep(5 * time.Second)
	kubeletStub.Lock()
	devicePluginSock := path.Join("/tmp", kubeletStub.pluginEndpoint)
	kubeletStub.Unlock()
	// Verifies the grpcServer is ready to serve services.
	conn, err := grpc.Dial(devicePluginSock, grpc.WithInsecure(),
		grpc.WithDialer(func(addr string, timeout time.Duration) (net.Conn, error) {
			return net.DialTimeout("unix", addr, timeout)
		}))
	defer conn.Close()
	as.Nil(err)
	client := pluginapi.NewDevicePluginClient(conn)

	// Tests ListAndWatch
	testGpuManager.devices["dev1"] = pluginapi.Device{"dev1", pluginapi.Healthy}
	testGpuManager.devices["dev2"] = pluginapi.Device{"dev2", pluginapi.Healthy}
	stream, err := client.ListAndWatch(context.Background(), &pluginapi.Empty{})
	as.Nil(err)
	devs, err := stream.Recv()
	as.Nil(err)
	devices := make(map[string]*pluginapi.Device)
	for _, d := range devs.Devices {
		devices[d.ID] = d
	}
	as.NotNil(devices["dev1"])
	as.NotNil(devices["dev2"])

	// Tests Allocate
	resp, err := client.Allocate(context.Background(), &pluginapi.AllocateRequest{
		DevicesIDs: []string{"dev1"},
	})
	as.Nil(err)
	as.Len(resp.Spec, 1)
	resp, err = client.Allocate(context.Background(), &pluginapi.AllocateRequest{
		DevicesIDs: []string{"dev1", "dev2"},
	})
	as.Nil(err)
	as.Len(resp.Spec, 2)
	resp, err = client.Allocate(context.Background(), &pluginapi.AllocateRequest{
		DevicesIDs: []string{"dev1", "dev3"},
	})
	as.Nil(resp)
	as.NotNil(err)
}
