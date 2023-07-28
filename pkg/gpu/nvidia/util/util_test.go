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

package util

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDeviceNameFromPath(t *testing.T) {
	as := assert.New(t)
	name, err := DeviceNameFromPath("/dev/nvidia0")
	as.Nil(err)
	as.Equal("nvidia0", name)

	name, err = DeviceNameFromPath("/dev/somethingelse0")
	as.Error(err)
	as.Contains(err.Error(), "is not a valid GPU device path")
}

func TestMpsPinnedDeviceMemLimit(t *testing.T) {
	as := assert.New(t)
	limits := MpsPinnedDeviceMemLimit(3, uint64(900))

	as.Equal("0=900MB,1=900MB,2=900MB", limits)
}
