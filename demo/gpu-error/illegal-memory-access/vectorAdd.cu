/* Copyright 2017 Google Inc. All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
limitations under the License.
*/

/**
 * This sample does a very simple vector add, and will trigger illegal memory
 * access error. The purpose of this sample is to test the error handling of
 * the device plugin or other components.
 */

 #include <stdio.h>
 #include <cuda_runtime.h>
 #include <helper_cuda.h>

 /**
  * Computes the vector addition and intentionally triggers memory error
  */
 __global__ void
 vectorAddAndTriggerError(const float *A, const float *B, float *C, int numElements)
 {
     // Intentionally triggering out of bounds
     int i = (blockDim.x * blockIdx.x) + threadIdx.x + 1000000000000;
     C[i] = A[i] + B[i];
 }

 int main(void)
 {
     printf("Starting illegal memory access sample\n");
     // Error code to check return values for CUDA calls
     cudaError_t err = cudaSuccess;

     int vecLength = 50000;
     size_t size = vecLength * sizeof(float);

     // Initializing two vectors on host
     float *h_A = (float *)malloc(size);
     float *h_B = (float *)malloc(size);
     for (int i = 0; i < vecLength; ++i)
     {
         h_A[i] = rand()/(float)RAND_MAX;
         h_B[i] = rand()/(float)RAND_MAX;
     }

     // Allocating three vectors on device
     float *d_A = NULL;
     err = cudaMalloc((void **)&d_A, size);
     float *d_B = NULL;
     err = cudaMalloc((void **)&d_B, size);
     float *d_C = NULL;
     err = cudaMalloc((void **)&d_C, size);

     // copy data from host to device
     err = cudaMemcpy(d_A, h_A, size, cudaMemcpyHostToDevice);
     err = cudaMemcpy(d_B, h_B, size, cudaMemcpyHostToDevice);

     // Run the vectorAdd func and trigger error
     int threadsPerBlock = 256;
     int blocksPerGrid =(vecLength + threadsPerBlock - 1) / threadsPerBlock;
     printf("Run vectorAdd with %d blocks of %d threads\n", blocksPerGrid, threadsPerBlock);
     vectorAddAndTriggerError<<<blocksPerGrid, threadsPerBlock>>>(d_A, d_B, d_C, vecLength);
     err = cudaGetLastError();

     if (err != cudaSuccess)
     {
         fprintf(stderr, "Failed to launch vectorAdd kernel (error code %s)!\n", cudaGetErrorString(err));
         exit(EXIT_FAILURE);
     }

     printf("Copy results from the device to the host\n");
     float *h_C = (float *)malloc(size);
     err = cudaMemcpy(h_C, d_C, size, cudaMemcpyDeviceToHost);

     // Expecting error here
     if (err != cudaSuccess)
     {
         fprintf(stderr, "Failed to copy vector C from device to host (error code %s)!\n", cudaGetErrorString(err));
         exit(EXIT_FAILURE);
     }

     return 0;
 }
