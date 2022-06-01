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
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestValidateRequest(t *testing.T) {
	cases := []struct {
		name              string
		requestDevicesIDs []string
		deviceCount       int
		sharingStrategy   GPUSharingStrategy
		wantError         error
	}{{
		name:              "don't have virtual device IDs - both timesharing and mps",
		requestDevicesIDs: []string{"nvidia0", "nvidia1"},
		deviceCount:       1,
		wantError:         nil,
	}, {
		name:              "only have one physical device - mps",
		requestDevicesIDs: []string{"nvidia0/vgpu0", "nvidia0/vgpu1"},
		deviceCount:       1,
		sharingStrategy:   MPS,
		wantError:         nil,
	}, {
		name:              "only request one virtual device - both timesharing and mps",
		requestDevicesIDs: []string{"nvidia0/vgpu0"},
		deviceCount:       2,
		wantError:         nil,
	}, {
		name:              "request multiple virtual devices and have one physical devices - timesharing",
		requestDevicesIDs: []string{"nvidia0/vgpu0", "nvidia1/vgpu1"},
		deviceCount:       1,
		wantError:         errors.New("invalid request for sharing GPU (time-sharing), at most 1 nvidia.com/gpu can be requested on GPU nodes"),
	}, {
		name:              "request multiple virtual devices and have multiple physical devices - mps",
		requestDevicesIDs: []string{"nvidia0/vgpu0", "nvidia1/vgpu1"},
		sharingStrategy:   MPS,
		deviceCount:       2,
		wantError:         errors.New("invalid request for sharing GPU (MPS), at most 1 nvidia.com/gpu can be requested on multi-GPU nodes"),
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.sharingStrategy != MPS {
				tc.sharingStrategy = TimeSharing
			}
			SharingStrategy = tc.sharingStrategy
			err := ValidateRequest(tc.requestDevicesIDs, tc.deviceCount)
			if err != nil && tc.wantError != nil {
				if diff := cmp.Diff(tc.wantError.Error(), err.Error()); diff != "" {
					t.Error("unexpected error (-want, +got) = ", diff)
				}
			} else if err != nil {
				t.Error("unexpected error: ", err)
			} else if tc.wantError != nil {
				t.Error("unexpected want error:", err)
			}
		})
	}
}

func TestVirtualToPhysicalDeviceID(t *testing.T) {
	cases := []struct {
		name            string
		virtualDeviceID string
		wantDeviceID    string
		wantError       error
	}{{
		name:            "invalid virtual device ID",
		virtualDeviceID: "nvidia0",
		wantDeviceID:    "",
		wantError:       errors.New("virtual device ID (nvidia0) is not valid"),
	}, {
		name:            "virtual device ID for common cases",
		virtualDeviceID: "nvidia0/vgpu0",
		wantDeviceID:    "nvidia0",
		wantError:       nil,
	}, {
		name:            "only request one  virtual device",
		virtualDeviceID: "nvidia0/gi0/vgpu0",
		wantDeviceID:    "nvidia0/gi0",
		wantError:       nil,
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deviceID, err := VirtualToPhysicalDeviceID(tc.virtualDeviceID)
			if diff := cmp.Diff(tc.wantDeviceID, deviceID); diff != "" {
				t.Error("unexpected deviceID (-want, +got) = ", diff)
			}
			if err != nil && tc.wantError != nil {
				if diff := cmp.Diff(tc.wantError.Error(), err.Error()); diff != "" {
					t.Error("unexpected error (-want, +got) = ", diff)
				}
			} else if err != nil {
				t.Error("unexpected error: ", err)
			} else if tc.wantError != nil {
				t.Error("unexpected want error:", err)
			}
		})
	}
}
