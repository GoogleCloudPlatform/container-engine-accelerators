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

package metrics

/*
#cgo linux LDFLAGS: -ldl -Wl,--unresolved-symbols=ignore-in-object-files
#cgo darwin LDFLAGS: -ldl -Wl,-undefined,dynamic_lookup

#include <stddef.h>
#include <dlfcn.h>
#include <stdlib.h>

#include "../../../../vendor/github.com/NVIDIA/gpu-monitoring-tools/bindings/go/nvml/nvml.h"

// This function is here because the API provided by NVML is not very user
// friendly. This function can be used to get average utilization for a gpu.
//
// `uuid`: The uuid identifier of the target GPU device.
// `lastSeenTimeStamp`: Return average using samples with timestamp greather than this timestamp. Unix epoch in micro seconds.
// `averageUsage`: Reference in which average is returned.
//
// In my experiments, I found that NVML_GPU_UTILIZATION_SAMPLES buffer stores
// 100 samples that are uniformly spread with ~6 samples per second. So the
// buffer stores last ~16s of data.
nvmlReturn_t nvmlDeviceGetAverageUsage(char *uuid, unsigned long long lastSeenTimeStamp, unsigned int* averageUsage) {
  nvmlValueType_t sampleValType;

  // This will be set to the number of samples that can be queried. We would
  // need to allocate an array of this size to store the samples.
  unsigned int sampleCount;

  nvmlDevice_t device;
  nvmlReturn_t r = nvmlDeviceGetHandleByUUID(uuid, &device);
  if (r != NVML_SUCCESS) {
	  return r;
  }

  // Invoking this method with `samples` set to NULL sets the sampleCount.
  r = nvmlDeviceGetSamples(device, NVML_GPU_UTILIZATION_SAMPLES, lastSeenTimeStamp, &sampleValType, &sampleCount, NULL);
  if (r != NVML_SUCCESS) {
    return r;
  }
  // Allocate memory to store sampleCount samples.
  // In my experiments, the sampleCount at this stage was always 120 for
  // NVML_TOTAL_POWER_SAMPLES and 100 for NVML_GPU_UTILIZATION_SAMPLES
  nvmlSample_t* samples = (nvmlSample_t*) malloc(sampleCount * sizeof(nvmlSample_t));
  r = nvmlDeviceGetSamples(device, NVML_GPU_UTILIZATION_SAMPLES, lastSeenTimeStamp, &sampleValType, &sampleCount, samples);
  if (r != NVML_SUCCESS) {
    free(samples);
    return r;
  }
  int i = 0;
  unsigned int sum = 0;
  for (; i < sampleCount; i++) {
    sum += samples[i].sampleValue.uiVal;
  }
  *averageUsage = sum/sampleCount;
  free(samples);
  return r;
}
*/
import "C"
import (
	"fmt"
	"time"
	"unsafe"
)

// AverageGPUUtilization reports the average GPU utilization over the last 10 seconds.
func AverageGPUUtilization(uuid string, since time.Duration) (uint, error) {
	lastTs := C.ulonglong(time.Now().Add(-1*since).UnixNano() / 1000)
	uuidCStr := C.CString(uuid)
	var util C.uint
	r := C.nvmlDeviceGetAverageUsage(uuidCStr, lastTs, &util)
	C.free(unsafe.Pointer(uuidCStr))
	if r != C.NVML_SUCCESS {
		return 0, fmt.Errorf("failed to get GPU utilization for device %s, nvml return code: %v", uuid, r)

	}

	return uint(util), nil
}
