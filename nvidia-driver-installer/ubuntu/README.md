This directory contains the following files:

- `Dockerfile`
- `Makefile`
- `entrypoint.sh`
- `daemonset.yaml`

- `daemonset-preloaded.yaml`

The first three files contain code for creating a docker container that can be
used to install NVIDIA GPU drivers. The `daemonset.yaml` file can be used to run
that docker container on a Kubernetes cluster. This was an experimental thing.

From GKE v1.11.3, GKE added native production-ready support for installing
NVIDIA GPU drivers on Ubuntu nodes. The `daemonset-preloaded.yaml` file is used
to trigger that installation. See instructions to use GPUs from GKE at
https://cloud.google.com/kubernetes-engine/docs/how-to/gpus
