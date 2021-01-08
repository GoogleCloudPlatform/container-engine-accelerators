# Partition GPUs

Simple command line tool to partition the GPUs as specified in a GPU configuration file. The GPU configuration file specifies the desired partition size, and this tool will use nvidia-smi to create the maximum number of partitions on the desired size on the node.

## To build GPU partitoner image
From root of the repository, run:
  `docker build -f partition_gpu/Dockerfile .`

## To deploy GPU partitioner on all GPU nodes in GKE cluster
  `kubectl apply -f partition_gpu.yaml`