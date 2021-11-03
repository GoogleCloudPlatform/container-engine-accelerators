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

package time_sharing

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia/mig"
)

const (
	// TODO revisit the name of this solution.
	TimeSharing = "time-sharing"
)

// HasTimeSharingStrategy returns true if the input gpuSharingStrategy include time-sharing.
func HasTimeSharingStrategy(gpuSharingStrategy string) bool {
	// Slicing GPUSharingStrategy into strategies.
	// GPUSharingStrategy will look like "mig,time-sharing" in the future.
	strategies := strings.Split(gpuSharingStrategy, ",")
	for _, strategy := range strategies {
		if strategy == TimeSharing {
			return true
		}
	}
	return false
}

// TimeSharingRequestValidation will first check if the input device IDs are virtual device IDs, and then validate the request.
// A valid time-sharing solution request should meet the following conditions:
// 1. if there is only one physical device, it is valid to request multiple virtual devices in a single request.
// 2. if there are multiple physical devices, it is only valid to request one virtual device in a single request.
// Noted: in this validation, each compute unit will be regarded as a physical device in the MIG mode.
func TimeSharingRequestValidation(requestDevicesIDs []string, deviceCount int, migDeviceManager *mig.DeviceManager) error {
	if len(requestDevicesIDs) < 2 {
		return nil
	}

	if isVirtualDeviceIDForDefaultMode(requestDevicesIDs[0]) && deviceCount > 1 {
		return errors.New("invalid request for time-sharing solution, at most 1 nvidia.com/gpu can be requested when there are more than 1 physical GPUs in a node")
	} else if isVirtualDeviceIDForMIGMode(requestDevicesIDs[0]) {
		if migDeviceManager == nil {
			return errors.New("invalid request for time-sharing solution, node suppose to be in MIG mode, but can't find MIG device manager")
		} else if len(migDeviceManager.ListGPUPartitionDevices()) > 1 {
			return errors.New("invalid request for time-sharing solution, at most 1 nvidia.com/gpu can be requested when there are more than 1 MIG GPU partitions in a node")
		}
	}
	return nil
}

// TODO We might be able to re-use this function for MPS solution in the future.
// VirtualToPhysicalDeviceID takes a virtualDeviceID and transfers it to a physicalDeviceID.
func VirtualToPhysicalDeviceID(virtualDeviceID string) (string, error) {
	if !IsVirtualDeviceID(virtualDeviceID) {
		return "", fmt.Errorf("virtual device ID (%s) is not valid", virtualDeviceID)
	}

	vgpuRegex := regexp.MustCompile("/vgpu([0-9]+)$")
	return vgpuRegex.Split(virtualDeviceID, -1)[0], nil
}

// TODO We might be able to re-use this function for MPS solution in the future.
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
