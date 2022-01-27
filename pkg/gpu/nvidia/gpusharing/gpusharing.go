// Copyright 2021 Google Inc. All Rights Reserved.
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

package gpusharing

import (
	"errors"
	"fmt"
	"regexp"
)

// ValidateRequest will first check if the input device IDs are virtual device IDs, and then validate the request.
// A valid sharing request should meet the following conditions:
// 1. if there is only one physical device, it is valid to request multiple virtual devices in a single request.
// 2. if there are multiple physical devices, it is only valid to request one virtual device in a single request.
// Note: in this validation, each MIG partition will be regarded as a physical device.
func ValidateRequest(requestDevicesIDs []string, deviceCount int) error {
	if len(requestDevicesIDs) > 1 && IsVirtualDeviceID(requestDevicesIDs[0]) && deviceCount > 1 {
		return errors.New("invalid request for sharing GPU, at most 1 nvidia.com/gpu can be requested on multi-GPU nodes")
	}

	return nil
}

// VirtualToPhysicalDeviceID takes a virtualDeviceID and converts it to a physicalDeviceID.
func VirtualToPhysicalDeviceID(virtualDeviceID string) (string, error) {
	if !IsVirtualDeviceID(virtualDeviceID) {
		return "", fmt.Errorf("virtual device ID (%s) is not valid", virtualDeviceID)
	}

	vgpuRegex := regexp.MustCompile("/vgpu([0-9]+)$")
	return vgpuRegex.Split(virtualDeviceID, -1)[0], nil
}

// isVirtualDeviceID returns true if a input device ID comes from a virtual GPU device.
func IsVirtualDeviceID(virtualDeviceID string) bool {
	return isVirtualDeviceIDForDefaultMode(virtualDeviceID) || isVirtualDeviceIDForMIGMode(virtualDeviceID)
}

func isVirtualDeviceIDForDefaultMode(virtualDeviceID string) bool {
	// Generally, the virtualDeviceID will form as 'nvidia0/vgpu0', with the underlying physicalDeviceID as 'nvidia0'.
	validRegex := regexp.MustCompile("nvidia([0-9]+)\\/vgpu([0-9]+)$")
	return validRegex.MatchString(virtualDeviceID)
}

func isVirtualDeviceIDForMIGMode(virtualDeviceID string) bool {
	// In MIG case, the virtualDeviceID will form as `nvidia0/gi0/vgpu0`, with the underlying physicalDeviceID as 'nvidia0/gi0'.
	validMigRegex := regexp.MustCompile("nvidia([0-9]+)\\/gi([0-9]+)\\/vgpu([0-9]+)$")
	return validMigRegex.MatchString(virtualDeviceID)
}
