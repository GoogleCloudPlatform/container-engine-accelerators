# GPUDirect-TCPX Release Notes
This release notes updates support for the following GPUDirect-TCPX components: GKE version, NCCL plugin installer, TCPX-daemon.

For new users, refer [Maximize GPU network bandwidth in Standard mode clusters](https://cloud.google.com/kubernetes-engine/docs/how-to/gpu-bandwidth-gpudirect-tcpx) to setup GPUDirect-TCPX enabled GKE clusters. This guide always installs the latest versions of GPUDirect-TCPX components.

For existing users, use this release notes to update your cluster with latest versions of GPUDirect-TCPX components.

For best practices, refer to [Best practice to run workload with GPUDirect-TCPX(O)](../gpudirect-tcpxo/best-practice.md).

## How to upgrade to a new release
#### Recommended GKE versions:
- When you want to upgrade NCCL plugin installer image and TCPX-daemon image, it is not a hard requirement to upgrade your GKE cluster and GKE node to the recommended GKE version. But recommended GKE versions have the best guarantee for compatibility.
- To upgrade GKE versions, refer to [Manually upgrading a cluster or node pool](https://cloud.google.com/kubernetes-engine/docs/how-to/upgrading-a-cluster) for general guides.
#### NCCL plugin installer image:
- Directly run `kubectl apply -f https://raw.githubusercontent.com/GoogleCloudPlatform/container-engine-accelerators/master/gpudirect-tcpx/nccl-tcpx-installer.yaml` to get your nccl-tcpx-installer daemonset updated. This manifest is always updated to use the latest NCCL plugin installer. The daemonset by default uses rolling upgrade strategies, and the upgrade process will be slow for a big nodepool. Consider delete the old daemonset and create a new one to accelerate the progress.
- Upgrading your NCCL plugin installer version does **NOT** need any VM recreation or reboot. However, pods running within the same workload need to use the same version of the NCCL plugin. Please ensure no workloads are being scheduled/running when applying this upgrade. Otherwise, pods within the same workload may have different NCCL plugin versions installed.
- This upgrade will upgrade the NCCL plugin version for **ALL** A3 High nodes in the cluster. If you only want to upgrade a specific nodepool, please update the [nodeSelector](https://github.com/GoogleCloudPlatform/container-engine-accelerators/blob/master/gpudirect-tcpx/nccl-tcpx-installer.yaml#L25-L29) before deploying the NCCL plugin installer manifest.
#### TCPX-daemon image:
- Update your tcpx-daemon with the new image when deploying your application.
- The tcpx-daemon version is coupled with the NCCL plugin installer version. Please ensure your NCCL plugin installer version is upgraded before applying this tcpx-daemon version upgrade to your applications.
#### Compatible NCCL version:
- The NCCL plugin installer includes NCCL core as well and it is recommended to use this NCCL core.
- If you need to use the open-source NCCL core, please use the compatible NCCL version for best performance.

## Releases
- [Jun 22, 2026](./README.md#jun-22-2026)
- [Feb 15, 2024](./README.md#feb-15-2024)

## Jun 22, 2026
#### NCCL plugin installer image:
```
us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpx/nccl-plugin-gpudirecttcpx-dev:v3.1.12
```
#### TCPX-daemon image:
```
us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpx/tcpgpudmarxd-dev:v2.0.15
```
#### Compatible NCCL version:
```
Default NCCL version: nccl-2.19, which is provided by the NCCL plugin installer
qualified and supported: NCCL 2.19.4
```
#### NCCL configs:
```
"LD_LIBRARY_PATH=\"${LD_LIBRARY_PATH}:/usr/local/tcpx/lib64\"",
"NCCL_SOCKET_IFNAME=eth0",
"NCCL_ALGO=Ring,Tree",
"NCCL_PROTO=Simple",
"NCCL_CROSS_NIC=0",
"NCCL_NET_GDR_LEVEL=PIX",
"NCCL_P2P_PXN_LEVEL=0",
"NCCL_GPUDIRECTTCPX_SOCKET_IFNAME=eth1,eth2,eth3,eth4",
"NCCL_GPUDIRECTTCPX_CTRL_DEV=eth0",
"NCCL_DYNAMIC_CHUNK_SIZE=524288",
"NCCL_P2P_NET_CHUNKSIZE=524288",
"NCCL_P2P_PCI_CHUNKSIZE=524288",
"NCCL_P2P_NVL_CHUNKSIZE=1048576",
"NCCL_BUFFSIZE=4194304",
"NCCL_NSOCKS_PERTHREAD=4",
"NCCL_SOCKET_NTHREADS=1",
"NCCL_GPUDIRECTTCPX_TX_BINDINGS=eth1:8-21,112-125;eth2:8-21,112-125;eth3:60-73,164-177;eth4:60-73,164-177",
"NCCL_GPUDIRECTTCPX_RX_BINDINGS=eth1:22-35,126-139;eth2:22-35,126-139;eth3:74-87,178-191;eth4:74-87,178-191",
"NCCL_GPUDIRECTTCPX_PROGRAM_FLOW_STEERING_WAIT_MICROS=500000"
```
#### What is new within release:
* Update GPUDirect-TCPX components to NCCL plugin v3.1.12 and RxDM v2.0.15.
* Improved stability and performance for A3 High GKE clusters.

## Feb 15, 2024
#### NCCL plugin installer image:
```
us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpx/nccl-plugin-gpudirecttcpx-dev:v3.1.9
```
#### TCPX-daemon image:
```
us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpx/tcpgpudmarxd-dev:v2.0.12
```
#### Compatible NCCL version:
```
Default NCCL version: nccl-2.19, which is provided by the NCCL plugin installer
qualified and supported: NCCL 2.19.3
```
#### NCCL configs:
```
"LD_LIBRARY_PATH=\"${LD_LIBRARY_PATH}:/usr/local/tcpx/lib64\"",
"NCCL_SOCKET_IFNAME=eth0",
"NCCL_ALGO=Ring",
"NCCL_PROTO=Simple",
"NCCL_CROSS_NIC=0",
"NCCL_NET_GDR_LEVEL=PIX",
"NCCL_P2P_PXN_LEVEL=0",
"NCCL_GPUDIRECTTCPX_SOCKET_IFNAME=eth1,eth2,eth3,eth4",
"NCCL_GPUDIRECTTCPX_CTRL_DEV=eth0",
"NCCL_DYNAMIC_CHUNK_SIZE=524288",
"NCCL_P2P_NET_CHUNKSIZE=524288",
"NCCL_P2P_PCI_CHUNKSIZE=524288",
"NCCL_P2P_NVL_CHUNKSIZE=1048576",
"NCCL_BUFFSIZE=4194304",
"NCCL_NSOCKS_PERTHREAD=4",
"NCCL_SOCKET_NTHREADS=1",
"NCCL_GPUDIRECTTCPX_TX_BINDINGS=eth1:8-21,112-125;eth2:8-21,112-125;eth3:60-73,164-177;eth4:60-73,164-177",
"NCCL_GPUDIRECTTCPX_RX_BINDINGS=eth1:22-35,126-139;eth2:22-35,126-139;eth3:74-87,178-191;eth4:74-87,178-191",
"NCCL_GPUDIRECTTCPX_PROGRAM_FLOW_STEERING_WAIT_MICROS=500000"
```
