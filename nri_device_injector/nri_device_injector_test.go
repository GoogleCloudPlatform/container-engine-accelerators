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
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"golang.org/x/sys/unix"
)

func TestToNRIDevice(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "device-test")
	if err != nil {
		t.Fatalf("faield to make temporary device-test directory %s: %v", tempDir, err)
	}
	defer os.RemoveAll(tempDir)

	tests := map[string]struct {
		devPath  string
		devMajor int
		devMinor int
		devType  uint32
	}{
		"char nvidia0 device": {
			devPath:  "/nvidia0",
			devMajor: 195,
			devMinor: 0,
			devType:  unix.S_IFCHR,
		},
		"char nvidiactl device": {
			devPath:  "/nvidiactl",
			devMajor: 195,
			devMinor: 255,
			devType:  unix.S_IFCHR,
		},
		"char nvidia-uvm device": {
			devPath:  "/nvidia-uvm",
			devMajor: 100,
			devMinor: 100,
			devType:  unix.S_IFCHR,
		},
		"block foo device": {
			devPath:  "/foo",
			devMajor: 100,
			devMinor: 100,
			devType:  unix.S_IFBLK,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			devPath := tempDir + tc.devPath
			devNum := unix.Mkdev(uint32(tc.devMajor), uint32(tc.devMinor))
			if err := unix.Mknod(devPath, tc.devType|0644, int(devNum)); err != nil {
				t.Fatalf("failed to mknod for %s: %v ", devPath, err)
			}
			dev := device{Path: devPath}
			nriDevice, err := dev.toNRIDevice()

			wantType := ""
			switch tc.devType {
			case unix.S_IFBLK:
				wantType = blockDevice
			case unix.S_IFCHR:
				wantType = charDevice
			case unix.S_IFIFO:
				wantType = fifoDevice
			}
			assert.NoError(t, err)
			assert.Equalf(t, devPath, nriDevice.Path, "error asserting device dev path, expected %s, got %s", devPath, nriDevice.Path)
			assert.Equalf(t, wantType, nriDevice.Type, "error asserting device type, expected %s, got %s", wantType, nriDevice.Type)
			assert.Equalf(t, int64(tc.devMajor), nriDevice.Major, "error asserting device major, expected %d, got %d", int64(tc.devMajor), nriDevice.Major)
			assert.Equalf(t, int64(tc.devMinor), nriDevice.Minor, "error asserting device minor, expected %d, got %d", int64(tc.devMinor), nriDevice.Minor)
		})
	}
}

func TestGetDevices(t *testing.T) {
	tests := map[string]struct {
		container   string
		annotations map[string]string
		want        []device
		wantErr     bool
	}{
		"No device annotations": {
			container: "test",
		},
		"Empty annotation": {
			container:   "test",
			annotations: map[string]string{},
		},
		"Unrelated device annotation": {
			container:   "test",
			annotations: map[string]string{"foo": "foo1"},
		},
		"One valid device annotation injecting to container": {
			container: "test",
			annotations: map[string]string{
				"devices.gke.io/container.test": `
- path: /dev/test
`}, want: []device{{
				Path: "/dev/test",
			}},
		},
		"Multiple valid device annotation injecting to container": {
			container: "test",
			annotations: map[string]string{
				"devices.gke.io/container.test": `
- path: /dev/test0
- path: /dev/test1
  major: 123
  minor: 456
- path: /dev/test2
  Type: b
`}, want: []device{{
				Path: "/dev/test0",
			}, {
				Path:  "/dev/test1",
				Major: 123,
				Minor: 456,
			}, {
				Path: "/dev/test2",
				Type: "b",
			}},
		},
		"Multiple same device's annotation returning multiple same device": {
			container: "test",
			annotations: map[string]string{
				"devices.gke.io/container.test": `
- path: /dev/test0
- path: /dev/test0
  major: 123
  minor: 456
- path: /dev/test0
  major: 456
  minor: 789
`}, want: []device{{
				Path: "/dev/test0",
			}},
		},
		"Invalid device annotation": {
			container: "test",
			annotations: map[string]string{
				"devices.gke.io/container.test": `
- path: /dev/test0
- path: /dev/test1
  major: foo
`}, wantErr: true,
		},
		"Invalid annotation yaml": {
			container: "test",
			annotations: map[string]string{
				"devices.gke.io/container.test": `
- path: /dev/test0
path: /dev/test1
`}, wantErr: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			devices, err := getDevices(tc.container, tc.annotations)
			if (err != nil) != tc.wantErr {
				t.Errorf("getDevices() error = %v, wantErr %v", err, tc.wantErr)
				return
			}
			if !tc.wantErr {
				assert.NoError(t, err)
				assert.EqualValues(t, tc.want, devices)
			}
		})
	}
}
