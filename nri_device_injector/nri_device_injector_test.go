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
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseDevices(t *testing.T) {
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
`}, want: []device{{
				Path: "/dev/test0",
			}, {
				Path:  "/dev/test0",
				Major: 123,
				Minor: 456,
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
