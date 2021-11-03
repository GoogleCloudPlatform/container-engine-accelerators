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
	"net"
	"path"
	"time"

	"github.com/golang/glog"
	"golang.org/x/net/context"
	"google.golang.org/grpc"

	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1alpha"

	"github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia/time_sharing"
)

type pluginServiceV1Alpha struct {
	ngm *nvidiaGPUManager
}

func (s *pluginServiceV1Alpha) ListAndWatch(emtpy *pluginapi.Empty, stream pluginapi.DevicePlugin_ListAndWatchServer) error {
	glog.Infoln("device-plugin: ListAndWatch start")
	if err := s.sendDevices(stream); err != nil {
		return err
	}
	for {
		select {
		case d := <-s.ngm.Health:
			glog.Infof("device-plugin: %s device marked as %s", d.ID, d.Health)
			s.ngm.SetDeviceHealth(d.ID, d.Health)
			if err := s.sendDevices(stream); err != nil {
				return err
			}
		}
	}
}

func (s *pluginServiceV1Alpha) Allocate(ctx context.Context, rqt *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	// Validate if it is requesting time-sharing GPU resources.
	// If it is, then validate if the request meets the time-sharing specific conditions.
	if err := time_sharing.TimeSharingRequestValidation(rqt.DevicesIDs, len(s.ngm.devices), &s.ngm.migDeviceManager); err != nil {
		return nil, err
	}
	resp := new(pluginapi.AllocateResponse)
	// Add all requested devices to Allocate Response
	for _, id := range rqt.DevicesIDs {
		// If we are using the time-sharing solution, the input deviceID will be a virtual Device ID.
		// We need to map it to the corresponding physical device ID.
		if time_sharing.HasTimeSharingStrategy(s.ngm.gpuConfig.GPUSharingConfig.GPUSharingStrategy) {
			physicalDeviceID, err := time_sharing.VirtualToPhysicalDeviceID(id)
			if err != nil {
				return nil, err
			}
			id = physicalDeviceID
		}
		dev, ok := s.ngm.devices[id]
		if !ok {
			return nil, fmt.Errorf("invalid allocation request with non-existing device %s", id)
		}
		if dev.Health != pluginapi.Healthy {
			return nil, fmt.Errorf("invalid allocation request with unhealthy device %s", id)
		}
		resp.Devices = append(resp.Devices, &pluginapi.DeviceSpec{
			HostPath:      path.Join(s.ngm.devDirectory, id),
			ContainerPath: path.Join(s.ngm.devDirectory, id),
			Permissions:   "mrw",
		})
	}
	// Add all default devices to Allocate Response
	for _, d := range s.ngm.defaultDevices {
		resp.Devices = append(resp.Devices, &pluginapi.DeviceSpec{
			HostPath:      d,
			ContainerPath: d,
			Permissions:   "mrw",
		})
	}

	for _, mountPath := range s.ngm.mountPaths {
		resp.Mounts = append(resp.Mounts, &pluginapi.Mount{
			HostPath:      mountPath.HostPath,
			ContainerPath: mountPath.ContainerPath,
			ReadOnly:      true,
		})
	}
	return resp, nil
}

func (s *pluginServiceV1Alpha) RegisterService() {
	pluginapi.RegisterDevicePluginServer(s.ngm.grpcServer, s)
}

// Act as a grpc client and register with the kubelet.
func RegisterWithKubelet(kubeletEndpoint, pluginEndpoint, resourceName string) error {
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

func (s *pluginServiceV1Alpha) sendDevices(stream pluginapi.DevicePlugin_ListAndWatchServer) error {
	resp := new(pluginapi.ListAndWatchResponse)
	for _, dev := range s.ngm.ListDevices() {
		resp.Devices = append(resp.Devices, &pluginapi.Device{ID: dev.ID, Health: dev.Health})
	}
	glog.Infof("ListAndWatch: send devices %v\n", resp)
	if err := stream.Send(resp); err != nil {
		glog.Errorf("device-plugin: cannot update device states: %v\n", err)
		s.ngm.grpcServer.Stop()
		return err
	}
	return nil
}
