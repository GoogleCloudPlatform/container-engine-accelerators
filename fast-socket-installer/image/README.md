# NCCL Fast Socket Installer Image

The Dockerfile downloads and installs a specific stable release of NCCL Fast
Socket (see documentation [here](https://github.com/google/nccl-fastsocket)).

## To build NCCL Fast Socket Installer Image
From root of the repository, run:
  `docker build -f fast-socket-installer/image/Dockerfile .`