#include <cuda.h>
#include <cuda_runtime.h>

#include <dlfcn.h>
#include <errno.h>
#include <stdio.h>
#include <stdlib.h>
#include <unistd.h>

// #define CHECK(x, err, errcode) do { \
//   int retval = (x); \
//   if (retval != 1) { \
//     fprintf(stderr, "Error: %s returned %d at %s:%d\n", #x, retval, __FILE__, __LINE__); \
//     return(errcode); \
//   } \
// } while (0)


int main(int argc, const char **argv)
{
        int deviceCount = 0;
        cudaError_t error_id = cudaGetDeviceCount(&deviceCount);
        if (error_id != cudaSuccess)
        {
                fprintf(stderr, "cudaGetDeviceCount returned: %s\n", cudaGetErrorString(error_id));
                return 1;
        }

        if (deviceCount == 0)
        {
                printf("No GPU devices found\n");
                return 1;
        }

        for (int i = 0; i < deviceCount; i++)
        {
                cudaSetDevice(i);
          // Get memory info, total memory and the memory can be used in this client.
                // size_t freeMem = 0;
                // size_t totalMem = 0;
                // error_id = cudaMemGetInfo(&freeMem, &totalMem);
                // if (error_id != cudaSuccess)
                // {
                //         fprintf(stderr, "cudaMemGetInfo returned: %s\n", cudaGetErrorString(error_id));
                // 	return 1;
                // }
                // printf("For device %d ;  Free memory: %ld M, Total memory: %ld M\n", i, freeMem/(1024*1024), totalMem/(1024*1024));
           // Get device info, the multi process count can be used in this client.
                // #ifdef __cplusplus
                //     cudaDeviceProp deviceProp;
                //  #else // !_cplusplus
                //     struct cudaDeviceProp deviceProp;
                //  #endif
                // error_id = cudaGetDeviceProperties(&deviceProp, i);
                // if (error_id != cudaSuccess)
                // {
                //         fprintf(stderr, "cudaGetDeviceProperties returned: %s\n", cudaGetErrorString(error_id));
                // 	return 1;
                // }
                // printf("For device %d ;  multiProcessorCount: %d M", i, deviceProp.multiProcessorCount);
        }
        return 0;
}
