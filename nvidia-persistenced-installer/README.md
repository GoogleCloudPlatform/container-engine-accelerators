## To build Confidential GPU NVIDIA Persistence Daemon Installer Image
From root of the repository, run:
  `docker buildx build --pull --load -f nvidia-persistenced-installer/Dockerfile -t ${REGISTRY}/${IMAGE}:${TAG} .`
