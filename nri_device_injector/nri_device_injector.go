// Copyright 2023 Google Inc. All Rights Reserved.
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
	"context"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
	"sigs.k8s.io/yaml"

	"github.com/containerd/nri/pkg/api"
	"github.com/containerd/nri/pkg/stub"
	log "github.com/sirupsen/logrus"
)

const (
	deviceKeyPrefix = "devices.gke.io"
	// Key prefix for device injection to a container, followed by container name
	ctrDeviceKeyPrefix = deviceKeyPrefix + "/container."
	pluginName         = "device_injector_nri"
	pluginIdx          = "10"
	// Device types.
	blockDevice = "b"
	charDevice  = "c"
	fifoDevice  = "p"
)

type device struct {
	Path     string `json:"path"`
	Type     string `json:"type"`
	Major    int64  `json:"major"`
	Minor    int64  `json:"minor"`
	FileMode uint32 `json:"file_mode"`
	UID      uint32 `json:"uid"`
	GID      uint32 `json:"gid"`
}

type plugin struct {
	stub stub.Stub
}

func main() {
	var (
		opts []stub.Option
		err  error
	)

	opts = append(opts, stub.WithPluginName(pluginName))
	opts = append(opts, stub.WithPluginIdx(pluginIdx))

	p := &plugin{}

	if p.stub, err = stub.New(p, append(opts, stub.WithOnClose(p.onClose))...); err != nil {
		log.Errorf("Failed to create plugin stub: %v", err)
		os.Exit(1)
	}

	err = p.stub.Run(context.Background())
	if err != nil {
		log.Errorf("plugin exited with error %v", err)
		os.Exit(1)
	}
}

func (p *plugin) onClose() {
	log.Info("NRI connection closed")
}

// CreateContainer handles CreateContainer requests relayed to the plugin by containerd NRI.
// The plugin makes adjustment on containers with device injection annotations.
// When multiple annotations annotate devices with the same path, only the first one will be injected.
func (p *plugin) CreateContainer(_ context.Context, pod *api.PodSandbox, container *api.Container) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	if pod == nil {
		return nil, nil, nil
	}

	var (
		ctrName = container.Name
		l       = log.WithFields(log.Fields{"container": ctrName, "pod": pod.Name, "namespace": pod.Namespace})

		devices []device
		err     error
	)

	l.Info("Started CreateContainer")
	devices, err = getDevices(ctrName, pod.Annotations)
	if err != nil {
		return nil, nil, err
	}
	adjust := &api.ContainerAdjustment{}

	if len(devices) == 0 {
		l.Debug("No devices annotated...")
		return adjust, nil, nil
	}
	for _, d := range devices {
		l.WithField("device", d.Path).Info("Annotated device")
		deviceNRI, err := d.toNRIDevice()
		if err != nil {
			return nil, nil, err
		}
		adjust.AddDevice(deviceNRI)
		l.WithField("device", d.Path).Info("Injected device")
	}
	l.Info("Finished CreateContainer")
	return adjust, nil, nil
}

// getDevices returns parsed devices from pod annotations of device injections.
func getDevices(ctrName string, podAnnotations map[string]string) ([]device, error) {
	var (
		deviceKey string = ctrDeviceKeyPrefix + ctrName

		annotation    []byte
		parsedDevices []device
		devices       []device
	)

	if value, ok := podAnnotations[deviceKey]; ok {
		annotation = []byte(value)
	}
	if annotation == nil {
		return nil, nil
	}

	if err := yaml.Unmarshal(annotation, &parsedDevices); err != nil {
		return nil, fmt.Errorf("invalid device annotation %q: %w", deviceKey, err)
	}
	paths := make(map[string]bool)
	for _, d := range parsedDevices {
		if _, got := paths[d.Path]; got {
			continue
		} else {
			paths[d.Path] = true
			devices = append(devices, d)
		}
	}
	return devices, nil
}

// toNRIDevice retrieves device's major, minor and type from its path, and returns a NRI device
func (d *device) toNRIDevice() (*api.LinuxDevice, error) {
	var (
		stat unix.Stat_t
	)
	if err := unix.Lstat(d.Path, &stat); err != nil {
		return nil, err
	}

	var (
		devNumber = uint64(stat.Rdev)
		major     = unix.Major(devNumber)
		minor     = unix.Minor(devNumber)
		mode      = stat.Mode
		devType   string
	)
	switch mode & unix.S_IFMT {
	case unix.S_IFBLK:
		devType = blockDevice
	case unix.S_IFCHR:
		devType = charDevice
	case unix.S_IFIFO:
		devType = fifoDevice
	default:
		return nil, fmt.Errorf("invalid device type %v from device path %v", mode, d.Path)
	}
	apiDev := &api.LinuxDevice{
		Path:  d.Path,
		Type:  devType,
		Major: int64(major),
		Minor: int64(minor),
	}
	if d.FileMode != 0 {
		apiDev.FileMode = api.FileMode(d.FileMode)
	}
	if d.UID != 0 {
		apiDev.Uid = api.UInt32(d.UID)
	}
	if d.GID != 0 {
		apiDev.Gid = api.UInt32(d.GID)
	}
	return apiDev, nil
}
