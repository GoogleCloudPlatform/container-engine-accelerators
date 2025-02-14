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

#include "../../../../vendor/github.com/NVIDIA/go-nvml/pkg/nvml/nvml.h"

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
nvmlReturn_t nvmlDeviceGetAverageUsage(char *uuid, unsigned long long lastSeenTimeStamp, unsigned int* averageUsage, unsigned int* sumValue, unsigned int* sampleCountValue) {
  nvmlValueType_t sampleValType;

  // This will be set to the number of samples that can be queried. We would
  // need to allocate an array of this size to store the samples.
  unsigned int sampleCount;

  nvmlDevice_t device;
  nvmlReturn_t r = nvmlDeviceGetHandleByUUID(uuid, &device);
  if (r != NVML_SUCCESS) {
	  return r;
  }

  // Invoking this method with `samples` set to NULL, to get the size of samples that user needs to allocate.
  // The returned samplesCount will provide the number of samples that can be queried. The user needs to
  // allocate the buffer with size as samplesCount * sizeof(nvmlSample_t).
  r = nvmlDeviceGetSamples(device, NVML_GPU_UTILIZATION_SAMPLES, lastSeenTimeStamp, &sampleValType, &sampleCount, NULL);
  if (r != NVML_SUCCESS) {
    // @return
    //      - \ref NVML_SUCCESS                 if samples are successfully retrieved
    //      - \ref NVML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
    //      - \ref NVML_ERROR_INVALID_ARGUMENT  if \a device is invalid, \a samplesCount is NULL or
    //                                          reference to \a sampleCount is 0 for non null \a samples
    //      - \ref NVML_ERROR_NOT_SUPPORTED     if this query is not supported by the device
    //      - \ref NVML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
    //      - \ref NVML_ERROR_NOT_FOUND         if sample entries are not found
    //      - \ref NVML_ERROR_UNKNOWN           on any unexpected error
    return r;
  }
  // Allocate memory to store sampleCount samples.
  // In my experiments, the sampleCount at this stage was always 120 for
  // NVML_TOTAL_POWER_SAMPLES and 100 for NVML_GPU_UTILIZATION_SAMPLES
  // The reference to a sampleCount will not be 0, otherwise if will have NVML_ERROR_INVALID_ARGUMENT error
  nvmlSample_t* samples = (nvmlSample_t*) malloc(sampleCount * sizeof(nvmlSample_t));
  r = nvmlDeviceGetSamples(device, NVML_GPU_UTILIZATION_SAMPLES, lastSeenTimeStamp, &sampleValType, &sampleCount, samples);
  if (r != NVML_SUCCESS) {
    free(samples);
    return r;
  }
  int i = 0;
  unsigned int sum = 0;
  for (; i < sampleCount; i++) {
  // Power, Utilization and Clock samples are returned as type "unsigned int" for the union nvmlValue_t.
    sum += samples[i].sampleValue.uiVal;
  }
  *averageUsage = sum/sampleCount;
  *sumValue = sum;
  *sampleCountValue = sampleCount;
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
	var sum C.uint
	var sampleCount C.uint
	r := C.nvmlDeviceGetAverageUsage(uuidCStr, lastTs, &util, &sum, &sampleCount)
	C.free(unsafe.Pointer(uuidCStr))
	if r != C.NVML_SUCCESS {
		return 0, fmt.Errorf("failed to get GPU utilization for device %s, nvml return code: %v", uuid, r)
	}
	averageUtilization := uint(util)
	if averageUtilization > 100 || averageUtilization < 0 {
		return 0, fmt.Errorf("failed to get GPU utilization for device %s, out of range [0, 100] utilization: %d, sum: %d, sampleCount: %d", uuid, averageUtilization, uint(sum), uint(sampleCount))
	}
	return averageUtilization, nil
}
