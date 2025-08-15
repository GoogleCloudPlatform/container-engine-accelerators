# GPUDirect-RDMA Release Notes
This release notes updates support for the following GPUDirect-RDMA components: GKE version, NCCl plugin installer.

For new users, refer [Create a custom AI-optimized GKE cluster](https://cloud.google.com/ai-hypercomputer/docs/create/gke-ai-hypercompute-custom) to setup GPUDirect-RDMA enabled GKE clusters. This guide always installs the latest versions of GPUDirect-RDMA components.

For existing users, use this release notes to update your cluster with the latest versions of GPUDirect-RDMA components.

## How to upgrade to a new release
#### Recommended GKE versions:
- When you want to upgrade NCCL plugin installer image, it is not a hard requirement to upgrade your GKE cluster and GKE node to the recommended GKE version. But recommended GKE versions have the best guarantee for compatibility.
- To upgrade GKE versions, refer to [Manually upgrading a cluster or node pool](https://cloud.google.com/kubernetes-engine/docs/how-to/upgrading-a-cluster) for general guides.
#### NCCL plugin installer image:
- Directly run `kubectl apply -f https://raw.githubusercontent.com/GoogleCloudPlatform/container-engine-accelerators/refs/heads/master/gpudirect-rdma/nccl-rdma-installer.yaml` to get your nccl-rdma-installer daemonset updated. This manifest is always updated to use the latest NCCL plugin installer. The daemonset by default uses rolling upgrade strategies, and the upgrade process will be slow for a big nodepool. Consider delete the old daemonset and create a new one to accelerate the progress.
- Upgrading your NCCL plugin installer version does **NOT** need any VM recreation or reboot. However, pods running within the same workload need to use the same version of the NCCL plugin. Please ensure no workloads are being scheduled/running when applying this upgrade. Otherwise, pods within the same workload may have different NCCL plugin versions installed.
- This upgrade will upgrade the NCCL plugin version for **ALL** A3 Ultra and A4 nodes in the cluster. For A4X, please use `kubectl apply -f https://github.com/GoogleCloudPlatform/container-engine-accelerators/blob/master/gpudirect-rdma/nccl-rdma-installer-a4x.yaml` instead.



## Releases
- [Aug 15, 2025](./README.md#aug-15-2025)

## Aug 15, 2025
#### NCCL plugin installer image:
```
us-docker.pkg.dev/gce-ai-infra/gpudirect-gib/nccl-plugin-gib:v1.1.0
```
