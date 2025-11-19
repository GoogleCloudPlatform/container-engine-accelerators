# GPUDirect-TCPXO Release Notes
This release notes updates support for the following GPUDirect-TCPXO components: GKE version, NCCl plugin installer, TCPXO-daemon.

For new users, refer [Maximize GPU network bandwidth in Standard mode clusters](https://cloud.google.com/kubernetes-engine/docs/how-to/gpu-bandwidth-gpudirect-tcpx) to setup GPUDirect-TCPXO enabled GKE clusters. This guide always installs the latest versions of GPUDirect-TCPXO components.

For existing users, use this release notes to update your cluster with latest versions of GPUDirect-TCPXO components.

For best practices, refer to [Best practice to run workload with GPUDirect-TCPX(O)](./best-practice.md).

## How to upgrade to a new release
#### Recommended GKE versions:
- When you want to upgrade NCCL plugin installer image and TCPXO-daemon image, it is not a hard requirement to upgrade your GKE cluster and GKE node to the recommended GKE version. But recommended GKE versions have the best guarantee for compatibility.
- To upgrade GKE versions, refer to [Manually upgrading a cluster or node pool](https://cloud.google.com/kubernetes-engine/docs/how-to/upgrading-a-cluster) for general guides.
#### NCCL plugin installer image:
- Directly run `kubectl apply -f https://raw.githubusercontent.com/GoogleCloudPlatform/container-engine-accelerators/master/gpudirect-tcpxo/nccl-tcpxo-installer.yaml` to get your nccl-tcpxo-installer daemonset updated. This manifest is always updated to use the latest NCCL plugin installer. The daemonset by default uses rolling upgrade strategies, and the upgrade process will be slow for a big nodepool. Consider delete the old daemonset and create a new one to accelerate the progress.
- Upgrading your NCCL plugin installer version does **NOT** need any VM recreation or reboot. However, pods running within the same workload need to use the same version of the NCCL plugin. Please ensure no workloads are being scheduled/running when applying this upgrade. Otherwise, pods within the same workload may have different NCCL plugin versions installed.
- This upgrade will upgrade the NCCL plugin version for **ALL** A3 Mega nodes in the cluster. If you only want to upgrade a specific nodepool, please update the [nodeSelector](https://github.com/GoogleCloudPlatform/container-engine-accelerators/blob/master/gpudirect-tcpxo/nccl-tcpxo-installer.yaml#L25-L29) before deploying the NCCL plugin installer manifest.
#### TCPXO-daemon image:
- Update your tcpxo-daemon with the new image when deploying your application.
- The tcpxo-daemon version is coupled with the NCCL plugin installer version. Please ensure your NCCL plugin installer version is upgraded before applying this tcpxo-daemon version upgrade to your applications.
#### Compatible NCCL version:
- The NCCL plugin installer includes NCCL core as well and it is recommended to use this NCCL core.
- If you need to use the open-source NCCL core, please use the compatible NCCL version for best performance.
- To use open-source nccl core, update the following environment variables from `LD_LIBRARY_PATH=\"/usr/local/nvidia/lib64\"` to `LD_LIBRARY_PATH=\"${YOUR_OPEN_SOURCE_NCCL_CORE_PATH}:/usr/local/nvidia/lib64\"`
#### NCCL configs:
- NCCL configs are required for using GPUDirect-TCPX(O) feature. When deploying your workloads that use NCCL, set them as environment variables.
- Optionally, you can set all the configurations at once by following these steps:
  - Add the following key-value pair as an environment variable in your workload container manifest:
  ```
  NCCL_LIB_DIR="/usr/local/nvidia/lib64"
  ```
  - Ensure the `nccl-env-profile.sh` script is executed when your workload container starts. For example, you can do this in your Pod specification by overriding the container's command to include the following:
  ```
  source ${NCCL_LIB_DIR}/nccl-env-profile.sh
  ```



## Releases
- [Aug 21, 2025](./README.md#aug-21-2025)
- [Jun 27, 2025](./README.md#jun-27-2025)
- [May 01, 2025](./README.md#may-01-2025)
- [Feb 27, 2025](./README.md#feb-27-2025)
- [Feb 06, 2025](./README.md#feb-06-2025)
- [Nov 27, 2024](./README.md#nov-27-2024)
- [Oct 18, 2024](./README.md#oct-18-2024)
- [Sept 19, 2024](./README.md#sept-19-2024)
- [Sept 06, 2024](./README.md#sept-06-2024)
- [Aug 06, 2024](./README.md#aug-06-2024)
- [Jun 27, 2024](./README.md#jun-27-2024)
- [May 30, 2024](./README.md#may-30-2024)
- [May 20, 2024](./README.md#may-20-2024)
- [Apr 17, 2024](./README.md#apr-17-2024)

## Nov 19, 2025

#### NCCL plugin installer image:
```
us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpxo/nccl-plugin-gpudirecttcpx-dev:v1.0.14
```
#### TCPXO-daemon image:
```
us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpxo/tcpgpudmarxd-dev:v1.0.20
```
#### Compatible NCCL version:
```
Default NCCl version: nccl-2.28, which is provided by the NCCL plugin installer
qualified and supported: NCCL 2.28.7-1
```
#### NCCL configs:
```
"NCCL_FASTRAK_CTRL_DEV=eth0",
"NCCL_FASTRAK_IFNAME=eth1,eth2,eth3,eth4,eth5,eth6,eth7,eth8",
"NCCL_SOCKET_IFNAME=eth0",
"NCCL_CROSS_NIC=0",
"NCCL_PROTO=Simple,LL128",

"NCCL_MIN_NCHANNELS=4",
"NCCL_TUNER_PLUGIN=libnccl-tuner.so",
"NCCL_TUNER_CONFIG_PATH=/usr/local/tcpxo/lib64/a3plus_tuner_config.textproto",
# Please replace `/usr/local/tcpxo/lib64/` as the NCCL lib directory installd inside the workload container,
# the mounted library path are controlled in nccl installer destination, link. 
"NCCL_SHIMNET_GUEST_CONFIG_CHECKER_CONFIG_FILE=/usr/local/tcpxo/lib64/a3plus_guest_config.textproto",
# Please replace `/usr/local/tcpxo/lib64/` as the NCCL lib directory installed inside the workload container.
# the mounted library path are controlled in nccl installer destination, link.
"CUDA_VISIBLE_DEVICES=0,1,2,3,4,5,6,7",
"NCCL_NVLSTREE_MAX_CHUNKSIZE=131072",
"NCCL_P2P_NET_CHUNKSIZE=524288",
"NCCL_P2P_PCI_CHUNKSIZE=524288",
"NCCL_P2P_NVL_CHUNKSIZE=1048576",
"NCCL_FASTRAK_NUM_FLOWS=2",
"NCCL_FASTRAK_USE_SNAP=1",
"NCCL_FASTRAK_PLUGIN_ACCEPT_TIMEOUT_MS=600000",
"NCCL_FASTRAK_ENABLE_CONTROL_CHANNEL=0",
"NCCL_BUFFSIZE=8388608",
"NCCL_NET_GDR_LEVEL=PIX",
"NCCL_FASTRAK_ENABLE_HOTPATH_LOGGING=0",
"NCCL_FASTRAK_USE_LLCM=1",
"NCCL_NVLS_ENABLE=1"
"NCCL_DEBUG=WARN",
"NCCL_DEBUG_SUBSYS=INIT,NET,ENV,COLL,GRAPH"
```
#### What is new with in release:
* Retired the flag in the RxDM container, `num_nics`. Specifying this flag will not cause an error, but it no longer has any effect. 
* Added a script to process output of GPUViz to the NCCL plugin docker image.

## Aug 21, 2025

#### NCCL plugin installer image:
```
us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpxo/nccl-plugin-gpudirecttcpx-dev:v1.0.12
```
#### TCPXO-daemon image:
```
us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpxo/tcpgpudmarxd-dev:v1.0.18
```
#### Compatible NCCL version:
```
Default NCCl version: nccl-2.26, which is provided by the NCCL plugin installer
qualified and supported: NCCL 2.26.5
```
#### NCCL configs:
```
"NCCL_FASTRAK_CTRL_DEV=eth0",
"NCCL_FASTRAK_IFNAME=eth1,eth2,eth3,eth4,eth5,eth6,eth7,eth8",
"NCCL_SOCKET_IFNAME=eth0",
"NCCL_CROSS_NIC=0",
"NCCL_PROTO=Simple,LL128",

"NCCL_MIN_NCHANNELS=4",
"NCCL_TUNER_PLUGIN=libnccl-tuner.so",
"NCCL_TUNER_CONFIG_PATH=/usr/local/nvidia/lib64/a3plus_tuner_config.textproto",
# Please replace `/usr/local/nvidia/lib64/` as the NCCL lib directory installed inside the workload container,
# the mounted library path are controlled in nccl installer destination, link.
"NCCL_SHIMNET_GUEST_CONFIG_CHECKER_CONFIG_FILE=/usr/local/nvidia/lib64/a3plus_guest_config.textproto",
# Please replace `/usr/local/nvidia/lib64/` as the NCCL lib directory installed inside the workload container.
# the mounted library path are controlled in nccl installer destination, link.
"NCCL_NVLSTREE_MAX_CHUNKSIZE=131072",
"NCCL_DYNAMIC_CHUNK_SIZE=524288",
"NCCL_P2P_NET_CHUNKSIZE=524288",
"NCCL_P2P_PCI_CHUNKSIZE=524288",
"NCCL_P2P_NVL_CHUNKSIZE=1048576",
"NCCL_FASTRAK_NUM_FLOWS=2",
"NCCL_FASTRAK_USE_SNAP=1",
"NCCL_FASTRAK_PLUGIN_ACCEPT_TIMEOUT_MS=600000",
"NCCL_FASTRAK_ENABLE_CONTROL_CHANNEL=0",
"NCCL_BUFFSIZE=8388608",
"NCCL_NET_GDR_LEVEL=PIX",
"NCCL_FASTRAK_ENABLE_HOTPATH_LOGGING=0",
"NCCL_FASTRAK_USE_LLCM=1",
"NCCL_DEBUG=WARN",
```
#### What is new with in release:
* This release coincides with the open-source release of GPUDirect-TCPXO: [https://github.com/google/nccl-plugin-gpudirect-tcpxo](https://github.com/google/nccl-plugin-gpudirect-tcpxo)
  * The docker images provided are built from the open-source versions of RxDM and the NCCL network plugin.
  * These open-source based image are **drop-in** **replacements** for the previous closed-source releases.
* The open-source version of the RxDM image obsoletes two arguments.
  * Previous:

    ```
    --num_hops=2 --num_nics=8  --uid= --alsologtostderr
    ```

  * Current:

    ```
    --num_hops=2 --num_nics=8
    ```

    Please update your deployments to reflect these changes.

* This release now incorporates `NVLSTree` into the default configuration for the environment setup.
  * This follows from the Jun 27, 2025 release where we provided NVLSTree as an option.
  * For convenience and ease of transitioning, symlinks are still provided to the `_ll128` and `_nvlstree` profiles. Both of these are symlinks to the default profile.
  * The default `nccl-env-profile.sh` now sets:

    ```
    # This has been added, and must be set in your deployments:
    NCCL_NVLSTREE_MAX_CHUNKSIZE=131072
    # The following have been removed:
    # NCCL_NVLS_ENABLE=0
    # NCCL_ALGO=Ring,Tree
    ```

  * The guest config checker **will enforce** that `NCCL_NVLSTREE_MAX_CHUNKSIZE` is set to this value and will abort any workload where it is not set.
  * The presence of `NCCL_NVLS_ENABLE` or `NCCL_ALGO` will only be flagged as unexpected environment variables.
    * Moving forward, we recommend that `NCCL_ALGO` remains unset to allow NCCL core and the tuner plugin to select the most appropriate algorithm.
* If you do not wish to use NVLSTree nor LL128, you may use the `simple` profile (`nccl-env-profile-simple.sh`).
  * This profiles does not enable LL128 nor NVLSTree, and uses a tuner config (`a3plus_tuner_config_simple.textproto`) and guest config (`a3plus_guest_config_simple.textproto`) that work with them disabled..

## Jun 27, 2025

#### Important Note:
Starting from this release - [May 01, 2025](./README.md#may-01-2025), workloads have the following requirements:
```
glibc version 2.35 or higher
Ubuntu version 22.04 or higher
Debian version Debian 12 or higher
```

#### NCCL plugin installer image:
```
us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpxo/nccl-plugin-gpudirecttcpx-dev:v1.0.11
```
#### TCPXO-daemon image:
```
us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpxo/tcpgpudmarxd-dev:v1.0.17
```
#### Compatible NCCL version:
```
default NCCl version: nccl-2.23, which is provided by the NCCL plugin installer
qualified and supported: NCCL 2.23.4 
```
#### NCCL configs:
```
"NCCL_FASTRAK_CTRL_DEV=eth0",
"NCCL_FASTRAK_IFNAME=eth1,eth2,eth3,eth4,eth5,eth6,eth7,eth8",
"NCCL_SOCKET_IFNAME=eth0",
"NCCL_CROSS_NIC=0",
"NCCL_ALGO=Ring,Tree",
"NCCL_PROTO=Simple,LL128",

"NCCL_MIN_NCHANNELS=4",
"NCCL_TUNER_PLUGIN=libnccl-tuner.so",
"NCCL_TUNER_CONFIG_PATH=/usr/local/nvidia/lib64/a3plus_tuner_config.textproto",
# Please replace `/usr/local/nvidia/lib64/` as the NCCL lib directory installd inside the workload container,
# the mounted library path are controlled in nccl installer destination, link. 
"NCCL_SHIMNET_GUEST_CONFIG_CHECKER_CONFIG_FILE=/usr/local/nvidia/lib64/a3plus_guest_config.textproto",
# Please replace `/usr/local/nvidia/lib64/` as the NCCL lib directory installed inside the workload container.
# the mounted library path are controlled in nccl installer destination, link.
"NCCL_DYNAMIC_CHUNK_SIZE=524288",
"NCCL_P2P_NET_CHUNKSIZE=524288",
"NCCL_P2P_PCI_CHUNKSIZE=524288",
"NCCL_P2P_NVL_CHUNKSIZE=1048576",
"NCCL_FASTRAK_NUM_FLOWS=2",
"NCCL_FASTRAK_USE_SNAP=1",
"NCCL_FASTRAK_PLUGIN_ACCEPT_TIMEOUT_MS=600000",
"NCCL_FASTRAK_ENABLE_CONTROL_CHANNEL=0",
"NCCL_BUFFSIZE=8388608",
"NCCL_NET_GDR_LEVEL=PIX",
"NCCL_FASTRAK_ENABLE_HOTPATH_LOGGING=0",
"NCCL_FASTRAK_USE_LLCM=1",
"NCCL_NVLS_ENABLE=0"
"NCCL_DEBUG=WARN",
```
#### What is new with in release:
* This release adds support for NCCL NVLSTree Algorithm, which enables intra-node communication with NVLS Algorithm and inter-node communication with Tree Algorithm.
  * For Hypercomputing customers, NVLSTree may require host machine upgrades to reach optimal performance. Reach out to your customer service representative for more details.
  * To enable NVLSTree, please use: `nccl-env-profile-nvlstree.sh` instead of the default `nccl-env-profile.sh`, which will achieve the following environment setup:
  ```
  "NCCL_FASTRAK_CTRL_DEV=eth0",
  "NCCL_FASTRAK_IFNAME=eth1,eth2,eth3,eth4,eth5,eth6,eth7,eth8",
  "NCCL_SOCKET_IFNAME=eth0",
  "NCCL_CROSS_NIC=0",
  "NCCL_PROTO=Simple,LL128",
  "NCCL_MIN_NCHANNELS=4",
  "NCCL_TUNER_PLUGIN=libnccl-tuner.so",
  "NCCL_TUNER_CONFIG_PATH=/usr/local/nvidia/lib64/a3plus_tuner_config_nvlstree.textproto",
  "NCCL_SHIMNET_GUEST_CONFIG_CHECKER_CONFIG_FILE=/usr/local/nvidia/lib64/a3plus_guest_config_nvlstree.textproto",
  "CUDA_VISIBLE_DEVICES=0,1,2,3,4,5,6,7"
  "NCCL_NVLSTREE_MAX_CHUNKSIZE=131072"
  "NCCL_P2P_NET_CHUNKSIZE=524288",
  "NCCL_P2P_PCI_CHUNKSIZE=524288",
  "NCCL_P2P_NVL_CHUNKSIZE=1048576",
  "NCCL_FASTRAK_NUM_FLOWS=2",
  "NCCL_FASTRAK_USE_SNAP=1",
  "NCCL_FASTRAK_PLUGIN_ACCEPT_TIMEOUT_MS=600000",
  "NCCL_FASTRAK_ENABLE_CONTROL_CHANNEL=0",
  "NCCL_BUFFSIZE=8388608",
  "NCCL_NET_GDR_LEVEL=PIX",
  "NCCL_FASTRAK_ENABLE_HOTPATH_LOGGING=0",
  "NCCL_FASTRAK_USE_LLCM=1",
  "NCCL_DEBUG=WARN",
  "NCCL_DEBUG_SUBSYS=INIT,NET,ENV,COLL,GRAPH"
  ```


## May 01, 2025

#### Important Note:
Starting from this release, workloads have the following requirements:
```
glibc version 2.35 or higher
Ubuntu version 22.04 or higher
Debian version Debian 12 or higher
```

#### NCCL plugin installer image:
```
us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpxo/nccl-plugin-gpudirecttcpx-dev:v1.0.9-1
```
#### TCPXO-daemon image:
```
us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpxo/tcpgpudmarxd-dev:v1.0.15 
```
#### Compatible NCCL version:
```
default NCCl version: nccl-2.23, which is provided by the NCCL plugin installer
qualified and supported: NCCL 2.21.5-2.23.4 
```
#### NCCL configs:
```
"NCCL_FASTRAK_CTRL_DEV=eth0",
"NCCL_FASTRAK_IFNAME=eth1,eth2,eth3,eth4,eth5,eth6,eth7,eth8",
"NCCL_SOCKET_IFNAME=eth0",
"NCCL_CROSS_NIC=0",
"NCCL_ALGO=Ring,Tree",
"NCCL_PROTO=Simple,LL128",

"NCCL_MIN_NCHANNELS=4",
"NCCL_TUNER_PLUGIN=libnccl-tuner.so",
"NCCL_TUNER_CONFIG_PATH=/usr/local/nvidia/lib64/a3plus_tuner_config.textproto",
# Please replace `/usr/local/nvidia/lib64/` as the NCCL lib directory installd inside the workload container,
# the mounted library path are controlled in nccl installer destination, link. 
"NCCL_SHIMNET_GUEST_CONFIG_CHECKER_CONFIG_FILE=/usr/local/nvidia/lib64/a3plus_guest_config.textproto",
# Please replace `/usr/local/nvidia/lib64/` as the NCCL lib directory installed inside the workload container.
# the mounted library path are controlled in nccl installer destination, link.
"NCCL_DYNAMIC_CHUNK_SIZE=524288",
"NCCL_P2P_NET_CHUNKSIZE=524288",
"NCCL_P2P_PCI_CHUNKSIZE=524288",
"NCCL_P2P_NVL_CHUNKSIZE=1048576",
"NCCL_FASTRAK_NUM_FLOWS=2",
"NCCL_FASTRAK_USE_SNAP=1",
"NCCL_FASTRAK_PLUGIN_ACCEPT_TIMEOUT_MS=600000",
"NCCL_FASTRAK_ENABLE_CONTROL_CHANNEL=0",
"NCCL_BUFFSIZE=8388608",
"NCCL_NET_GDR_LEVEL=PIX",
"NCCL_FASTRAK_ENABLE_HOTPATH_LOGGING=0",
"NCCL_FASTRAK_USE_LLCM=1",
"NCCL_NVLS_ENABLE=0"
"NCCL_DEBUG=WARN",
```
#### What is new with in release:
* Following the release of v1.0.8 which introduced support for NCCL LL128, this release now makes LL128 the default tuning parameter. About NCCL LL128, check [Best practice to run workload with GPUDirect-TCPX(O)](./best-practice.md) for more details.
  * This change has been reflected in the provided `nccl-env-profile.sh`.
  * The new default profile sets the following environment variables:
  ```
  NCCL_PROTO=Simple,LL128
  NCCL_TUNER_CONFIG_PATH=/usr/local/nvidia/lib64/a3plus_tuner_config.textproto
  NCCL_SHIMNET_GUEST_CONFIG_CHECKER_CONFIG_FILE=/usr/local/nvidia/lib64/a3plus_guest_config.textproto
  ```
* Note that the default tuner configuration file (`a3plus_tuner_config.textproto`) and the config checker configuration file (`a3plus_guest_config.textproto`) expect `NCCL_PROTO=Simple,LL128` to be set.
  * To disable LL128, please source `nccl-env-profile-simple.sh` instead.
  * `nccl-env-profile-simple.sh ` links configuration files which do not leverage LL128 (`a3plus_tuner_config_simple.textproto` and `a3plus_guest_config_simple.textproto`)
* For convenience and ease of use, the following LL128 specific files from the previous release exist as symbolic links
  ```
  nccl-env-profile-ll128.sh
  a3plus_tuner_config_ll128.textproto
  a3plus_guest_config_ll128.textproto
  ```

## Feb 27, 2025
#### GKE 1.32 starts to support TCPXO:
```
For 1.32 >= 1.31.2-gke.1489001
```
## Feb 06, 2025
#### NCCL plugin installer image:
```
us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpxo/nccl-plugin-gpudirecttcpx-dev:v1.0.8-1
```
#### TCPXO-daemon image:
```
us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpxo/tcpgpudmarxd-dev:v1.0.14
```
#### Compatible NCCL version:
```
default NCCl version: nccl-2.23, which is provided by the NCCL plugin installer
qualified and supported: NCCL 2.21.5-2.23.4 
```
#### NCCL configs:
```
## required nccl configs.
"LD_LIBRARY_PATH=\"${LD_LIBRARY_PATH}:/usr/local/nvidia/lib64\"",
"NCCL_FASTRAK_CTRL_DEV=eth0",
"NCCL_FASTRAK_IFNAME=eth1,eth2,eth3,eth4,eth5,eth6,eth7,eth8",
"NCCL_SOCKET_IFNAME=eth0",
"NCCL_CROSS_NIC=0",
"NCCL_ALGO=Ring,Tree",
"NCCL_PROTO=Simple",
"NCCL_MIN_NCHANNELS=4",
"NCCL_TUNER_PLUGIN=libnccl-tuner.so",
"NCCL_TUNER_CONFIG_PATH=/usr/local/nvidia/lib64/a3plus_tuner_config.textproto",
"NCCL_SHIMNET_GUEST_CONFIG_CHECKER_CONFIG_FILE=/usr/local/nvidia/lib64/a3plus_guest_config.textproto",
"NCCL_DYNAMIC_CHUNK_SIZE=524288",
"NCCL_P2P_NET_CHUNKSIZE=524288",
"NCCL_P2P_PCI_CHUNKSIZE=524288",
"NCCL_P2P_NVL_CHUNKSIZE=1048576",
"NCCL_FASTRAK_NUM_FLOWS=2",
"NCCL_FASTRAK_USE_SNAP=1",
"NCCL_FASTRAK_PLUGIN_ACCEPT_TIMEOUT_MS=600000",
"NCCL_FASTRAK_ENABLE_CONTROL_CHANNEL=0",
"NCCL_BUFFSIZE=8388608",
"CUDA_VISIBLE_DEVICES=0,1,2,3,4,5,6,7",
"NCCL_NET_GDR_LEVEL=PIX",
"NCCL_FASTRAK_ENABLE_HOTPATH_LOGGING=0",
"NCCL_FASTRAK_USE_LLCM=1",
"NCCL_NVLS_ENABLE=0"
## recommended nccl configs, to log NCCL errors
"NCCL_DEBUG=WARN",
"NCCL_DEBUG_SUBSYS=INIT,NET,ENV,COLL,GRAPH"
```
#### What is new with in release:
* Support NCCL LL128 for small-medium sized collective performance improvements, including optimized NCCL tuning profile and updated guest configuration profiles. Refer to [Best practice to run workload with GPUDirect-TCPX(O)](./best-practice.md) for more details.
* Qualified a wider range of NCCL core version.

## Nov 27, 2024
#### Recommended GKE version:
```
For 1.28 >= 1.28.11-gke.1289000
For 1.29 >= 1.29.6-gke.1254000 
For 1.30 >= 1.30.4-gke.1348000
Starts to support 1.31, for 1.31 >= 1.31.1-gke.2008000
```
#### NCCL plugin installer image:
```
us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpxo/nccl-plugin-gpudirecttcpx-dev:v1.0.7
```
#### TCPXO-daemon image:
```
us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpxo/tcpgpudmarxd-dev:v1.0.13_1
```
#### Compatible NCCL version:
```
nccl-2.21.5
```
#### NCCL configs:
```
## required nccl configs.
"LD_LIBRARY_PATH=\"${LD_LIBRARY_PATH}:/usr/local/nvidia/lib64\"",
"NCCL_FASTRAK_CTRL_DEV=eth0",
"NCCL_FASTRAK_IFNAME=eth1,eth2,eth3,eth4,eth5,eth6,eth7,eth8",
"NCCL_SOCKET_IFNAME=eth0",
"NCCL_CROSS_NIC=0",
"NCCL_ALGO=Ring,Tree",
"NCCL_PROTO=Simple",
"NCCL_MIN_NCHANNELS=4",
"NCCL_TUNER_PLUGIN=libnccl-tuner.so",
"NCCL_TUNER_CONFIG_PATH=/usr/local/nvidia/lib64/a3plus_tuner_config.textproto",
"NCCL_SHIMNET_GUEST_CONFIG_CHECKER_CONFIG_FILE=/usr/local/nvidia/lib64/a3plus_guest_config.textproto",
"NCCL_DYNAMIC_CHUNK_SIZE=524288",
"NCCL_P2P_NET_CHUNKSIZE=524288",
"NCCL_P2P_PCI_CHUNKSIZE=524288",
"NCCL_P2P_NVL_CHUNKSIZE=1048576",
"NCCL_FASTRAK_NUM_FLOWS=2",
"NCCL_FASTRAK_USE_SNAP=1",
"NCCL_FASTRAK_PLUGIN_ACCEPT_TIMEOUT_MS=600000",
"NCCL_FASTRAK_ENABLE_CONTROL_CHANNEL=0",
"NCCL_BUFFSIZE=8388608",
"CUDA_VISIBLE_DEVICES=0,1,2,3,4,5,6,7",
"NCCL_NET_GDR_LEVEL=PIX",
"NCCL_FASTRAK_ENABLE_HOTPATH_LOGGING=0",
"NCCL_FASTRAK_USE_LLCM=1",
"NCCL_NVLS_ENABLE=0"
## recommended nccl configs, to log NCCL errors
"NCCL_DEBUG=WARN",
"NCCL_DEBUG_SUBSYS=INIT,NET,ENV,COLL,GRAPH"
```
#### What is new with in release:
* All RxDM logs are now recorded in `/tmp/mtest_fastrak_gpumem_manager.INFO` with log rotation every 10 MB per file.
* Properly handle `SIGTERM` passed into RxDM container and terminate RxDM immediately.
* A critical fix which prevents the FasTrak stack to run out of BAR address space when running large scale workloads.
* An SCTP timeout increase which improves stability of the FasTrak stack in large scale workloads.

## Oct 18, 2024
#### Recommended GKE versions:
```
For 1.28 >= 1.28.11-gke.1289000
For 1.29 >= 1.29.6-gke.1254000
For 1.30 >= 1.30.4-gke.1348000
```
#### NCCL plugin installer image: 
```
us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpxo/nccl-plugin-gpudirecttcpx-dev:v1.0.6
```
#### TCPXO-daemon image: 
```
us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpxo/tcpgpudmarxd-dev:v1.0.12
```
#### Compatible NCCL version:
```
nccl-2.21.5
```
#### NCCL configs:
```
## required nccl configs.
"LD_LIBRARY_PATH=\"${LD_LIBRARY_PATH}:/usr/local/nvidia/lib64\"",
"NCCL_FASTRAK_CTRL_DEV=eth0",
"NCCL_FASTRAK_IFNAME=eth1,eth2,eth3,eth4,eth5,eth6,eth7,eth8",
"NCCL_SOCKET_IFNAME=eth0",
"NCCL_CROSS_NIC=0",
"NCCL_ALGO=Ring,Tree",
"NCCL_PROTO=Simple",
"NCCL_MIN_NCHANNELS=4",
"NCCL_TUNER_PLUGIN=libnccl-tuner.so",
"NCCL_TUNER_CONFIG_PATH=/usr/local/nvidia/lib64/a3plus_tuner_config.textproto",
"NCCL_SHIMNET_GUEST_CONFIG_CHECKER_CONFIG_FILE=/usr/local/nvidia/lib64/a3plus_guest_config.textproto",
"NCCL_DYNAMIC_CHUNK_SIZE=524288",
"NCCL_P2P_NET_CHUNKSIZE=524288",
"NCCL_P2P_PCI_CHUNKSIZE=524288",
"NCCL_P2P_NVL_CHUNKSIZE=1048576",
"NCCL_FASTRAK_NUM_FLOWS=2",
"NCCL_FASTRAK_USE_SNAP=1",
"NCCL_FASTRAK_PLUGIN_ACCEPT_TIMEOUT_MS=600000",
"NCCL_FASTRAK_ENABLE_CONTROL_CHANNEL=0",
"NCCL_BUFFSIZE=8388608",
"CUDA_VISIBLE_DEVICES=0,1,2,3,4,5,6,7",
"NCCL_NET_GDR_LEVEL=PIX",
"NCCL_FASTRAK_ENABLE_HOTPATH_LOGGING=0",
"NCCL_FASTRAK_USE_LLCM=1",
"NCCL_NVLS_ENABLE=0"
## recommended nccl configs, to log NCCL errors
"NCCL_DEBUG=WARN",
"NCCL_DEBUG_SUBSYS=INIT,NET,ENV,COLL,GRAPH"
```
#### What is new with in release:
* tcpxo-daemon resiliency improvements

## Sept 19, 2024
#### Compatible GKE versions:
```
For 1.28 >= 1.28.11-gke.1289000
For 1.29 >= 1.29.6-gke.1254000
For 1.30 >= 1.30.4-gke.1129000
```
#### NCCL plugin installer image: 
```
us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpxo/nccl-plugin-gpudirecttcpx-dev:v1.0.5
```
#### TCPXO-daemon image: 
```
us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpxo/tcpgpudmarxd-dev:v1.0.11
```
#### Compatible NCCL version:
```
nccl-2.21.5
```
#### NCCL configs:
```
## required
"LD_LIBRARY_PATH=\"${LD_LIBRARY_PATH}:/usr/local/nvidia/lib64\"",
"NCCL_FASTRAK_CTRL_DEV=eth0",
"NCCL_FASTRAK_IFNAME=eth1,eth2,eth3,eth4,eth5,eth6,eth7,eth8",
"NCCL_SOCKET_IFNAME=eth0",
"NCCL_CROSS_NIC=0",
"NCCL_ALGO=Ring,Tree",
"NCCL_PROTO=Simple",
"NCCL_MIN_NCHANNELS=4",
"NCCL_TUNER_PLUGIN=libnccl-tuner.so",
"NCCL_TUNER_CONFIG_PATH=/usr/local/nvidia/lib64/a3plus_tuner_config.textproto",
"NCCL_SHIMNET_GUEST_CONFIG_CHECKER_CONFIG_FILE=/usr/local/nvidia/lib64/a3plus_guest_config.textproto",
"NCCL_DYNAMIC_CHUNK_SIZE=524288",
"NCCL_P2P_NET_CHUNKSIZE=524288",
"NCCL_P2P_PCI_CHUNKSIZE=524288",
"NCCL_P2P_NVL_CHUNKSIZE=1048576",
"NCCL_FASTRAK_NUM_FLOWS=2",
"NCCL_FASTRAK_USE_SNAP=1",
"NCCL_FASTRAK_PLUGIN_ACCEPT_TIMEOUT_MS=600000",
"NCCL_FASTRAK_ENABLE_CONTROL_CHANNEL=0",
"NCCL_BUFFSIZE=8388608",
"CUDA_VISIBLE_DEVICES=0,1,2,3,4,5,6,7",
"NCCL_NET_GDR_LEVEL=PIX",
"NCCL_FASTRAK_ENABLE_HOTPATH_LOGGING=0",
"NCCL_FASTRAK_USE_LLCM=1",
"NCCL_NVLS_ENABLE=0"
## recommended, to log NCCL errors
"NCCL_DEBUG=WARN",
"NCCL_DEBUG_SUBSYS=INIT,NET,ENV,COLL,GRAPH"
```
#### What is new in this release:
* We recommend users to set the following NCCL debug related params to enable WARN logging level for better debuggability:
`NCCL_DEBUG=WARN`,
`NCCL_DEBUG_SUBSYS=INIT,NET,ENV,COLL,GRAPH`. Note: This setting might have some performance impact on certain workload, and might also increase log volume in failure cases.
* GKE startup probe support for the TCPXO-daemon. Refer to [Best practice to run workload with GPUDirect-TCPX(O)](./best-practice.md) for more details.


## Sept 06, 2024
#### GKE 1.30 starts to support TCPXO:
```
For 1.30 >= 1.30.4-gke.1129000
```

## Aug 06, 2024
#### Recommended GKE versions:
```
For 1.28 >= 1.28.11-gke.1289000
For 1.29 >= 1.29.6-gke.1254000
```
#### NCCL plugin installer image: 
```
us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpxo/nccl-plugin-gpudirecttcpx-dev:v1.0.4
```
#### TCPXO-daemon image: 
```
us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpxo/tcpgpudmarxd-dev:v1.0.10
```
#### Compatible NCCL version:
```
nccl-2.21.5
```
#### NCCL configs:
```
"LD_LIBRARY_PATH=\"${LD_LIBRARY_PATH}:/usr/local/nvidia/lib64\"",
"NCCL_FASTRAK_CTRL_DEV=eth0",
"NCCL_FASTRAK_IFNAME=eth1,eth2,eth3,eth4,eth5,eth6,eth7,eth8",
"NCCL_SOCKET_IFNAME=eth0",
"NCCL_CROSS_NIC=0",
"NCCL_ALGO=Ring,Tree",
"NCCL_PROTO=Simple",
"NCCL_MIN_NCHANNELS=4",
"NCCL_TUNER_PLUGIN=libnccl-tuner.so",
"NCCL_TUNER_CONFIG_PATH=/usr/local/nvidia/lib64/a3plus_tuner_config.textproto",
"NCCL_SHIMNET_GUEST_CONFIG_CHECKER_CONFIG_FILE=/usr/local/nvidia/lib64/a3plus_guest_config.textproto",
"NCCL_DYNAMIC_CHUNK_SIZE=524288",
"NCCL_P2P_NET_CHUNKSIZE=524288",
"NCCL_P2P_PCI_CHUNKSIZE=524288",
"NCCL_P2P_NVL_CHUNKSIZE=1048576",
"NCCL_FASTRAK_NUM_FLOWS=2",
"NCCL_FASTRAK_USE_SNAP=1",
"NCCL_FASTRAK_PLUGIN_ACCEPT_TIMEOUT_MS=600000",
"NCCL_FASTRAK_ENABLE_CONTROL_CHANNEL=0",
"NCCL_BUFFSIZE=8388608",
"CUDA_VISIBLE_DEVICES=0,1,2,3,4,5,6,7",
"NCCL_NET_GDR_LEVEL=PIX",
"NCCL_FASTRAK_ENABLE_HOTPATH_LOGGING=0",
"NCCL_FASTRAK_USE_LLCM=1",
"NCCL_NVLS_ENABLE=0"
```
#### What is new in this release:
* Report `ncclSystemError` instead of `ncclInternalError` to NCCL core when TCPXO software stack encounters errors for network operations.

## Jun 27, 2024
#### Recommended GKE versions:
```
For 1.28 >= 1.28.10-gke.1141000
For 1.29 >= 1.29.5-gke.1121000
```
#### NCCL plugin installer image: 
```
us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpxo/nccl-plugin-gpudirecttcpx-dev:v1.0.3
```
#### TCPXO-daemon image: 
```
us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpxo/tcpgpudmarxd-dev:v1.0.09
```
#### Compatible NCCL version:
```
nccl-2.21.5
```
#### NCCL configs:
```
"LD_LIBRARY_PATH=\"${LD_LIBRARY_PATH}:/usr/local/nvidia/lib64\"",
"NCCL_FASTRAK_CTRL_DEV=eth0",
"NCCL_FASTRAK_IFNAME=eth1,eth2,eth3,eth4,eth5,eth6,eth7,eth8",
"NCCL_SOCKET_IFNAME=eth0",
"NCCL_CROSS_NIC=0",
"NCCL_ALGO=Ring,Tree",
"NCCL_PROTO=Simple",
"NCCL_MIN_NCHANNELS=4",
"NCCL_TUNER_PLUGIN=libnccl-tuner.so",
"NCCL_TUNER_CONFIG_PATH=/usr/local/nvidia/lib64/a3plus_tuner_config.textproto",
"NCCL_SHIMNET_GUEST_CONFIG_CHECKER_CONFIG_FILE=/usr/local/nvidia/lib64/a3plus_guest_config.textproto",
"NCCL_DYNAMIC_CHUNK_SIZE=524288",
"NCCL_P2P_NET_CHUNKSIZE=524288",
"NCCL_P2P_PCI_CHUNKSIZE=524288",
"NCCL_P2P_NVL_CHUNKSIZE=1048576",
"NCCL_FASTRAK_NUM_FLOWS=2",
"NCCL_FASTRAK_USE_SNAP=1",
"NCCL_FASTRAK_PLUGIN_ACCEPT_TIMEOUT_MS=600000",
"NCCL_FASTRAK_ENABLE_CONTROL_CHANNEL=0",
"NCCL_BUFFSIZE=8388608",
"CUDA_VISIBLE_DEVICES=0,1,2,3,4,5,6,7",
"NCCL_NET_GDR_LEVEL=PIX",
"NCCL_FASTRAK_ENABLE_HOTPATH_LOGGING=0",
"NCCL_FASTRAK_USE_LLCM=1",
"NCCL_NVLS_ENABLE=0"
```
### What is new in this release:
* Add `NCCL_TUNER_CONFIG_PATH` config path validation. No-existing config file path will abort the workload process during startup.
* Add `--enforce_kernel_ipv6_support=false` as the default setting into the TCPXO-daemon startup script.
* A new demo script for 2 node NCCL allgather test.

## May 30, 2024
#### Recommended GKE versions:
```
For 1.28 >= 1.28.9-gke.1289000
For 1.29 >= 1.29.4-gke.1670000
```
#### NCCL plugin installer image: 
```
us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpxo/nccl-plugin-gpudirecttcpx-dev:v1.0.2
```
#### TCPXO-daemon image: 
```
us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpxo/tcpgpudmarxd-dev:v1.0.8
```
#### Compatible NCCL version:
```
nccl-2.21.5
```
#### NCCL configs:
```
"LD_LIBRARY_PATH=\"${LD_LIBRARY_PATH}:/usr/local/nvidia/lib64\"",
"NCCL_FASTRAK_CTRL_DEV=eth0",
"NCCL_FASTRAK_IFNAME=eth1,eth2,eth3,eth4,eth5,eth6,eth7,eth8",
"NCCL_SOCKET_IFNAME=eth0",
"NCCL_CROSS_NIC=0",
"NCCL_ALGO=Ring,Tree",
"NCCL_PROTO=Simple",
"NCCL_MIN_NCHANNELS=4",
"NCCL_TUNER_PLUGIN=libnccl-tuner.so",
"NCCL_SHIMNET_GUEST_CONFIG_CHECKER_CONFIG_FILE=/usr/local/nvidia/lib64/a3plus_guest_config.textproto",
"NCCL_DYNAMIC_CHUNK_SIZE=524288",
"NCCL_P2P_NET_CHUNKSIZE=524288",
"NCCL_P2P_PCI_CHUNKSIZE=524288",
"NCCL_P2P_NVL_CHUNKSIZE=1048576",
"NCCL_FASTRAK_NUM_FLOWS=2",
"NCCL_FASTRAK_USE_SNAP=1",
"NCCL_FASTRAK_PLUGIN_ACCEPT_TIMEOUT_MS=600000",
"NCCL_FASTRAK_ENABLE_CONTROL_CHANNEL=0",
"NCCL_BUFFSIZE=8388608",
"CUDA_VISIBLE_DEVICES=0,1,2,3,4,5,6,7",
"NCCL_NET_GDR_LEVEL=PIX",
"NCCL_FASTRAK_ENABLE_HOTPATH_LOGGING=0",
"NCCL_FASTRAK_USE_LLCM=1",
"NCCL_NVLS_ENABLE=0"
```
#### What is new in this release:  
* Two additional environment variables are configured for validation and stability:
`NCCL_SHIMNET_GUEST_CONFIG_CHECKER_CONFIG_FILE=/usr/local/nvidia/lib64/a3plus_guest_config.textproto`,
`NCCL_FASTRAK_PLUGIN_ACCEPT_TIMEOUT_MS=600000`

## May 20, 2024
#### Recommended GKE versions:
```
For 1.28 >= 1.28.9-gke.1289000
For 1.29 >= 1.29.4-gke.1670000
```
#### NCCL plugin installer image: 
```
us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpxo/nccl-plugin-gpudirecttcpx-dev:v1.0.1
```
#### TCPXO-daemon image: 
```
us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpxo/tcpgpudmarxd-dev:v1.0.8
```
#### Compatible NCCL version:
```
nccl-2.21.5
```
#### NCCL configs:
```
"LD_LIBRARY_PATH=\"${LD_LIBRARY_PATH}:/usr/local/nvidia/lib64\"",
"NCCL_FASTRAK_CTRL_DEV=eth0",
"NCCL_FASTRAK_IFNAME=eth1,eth2,eth3,eth4,eth5,eth6,eth7,eth8",
"NCCL_SOCKET_IFNAME=eth0",
"NCCL_CROSS_NIC=0",
"NCCL_ALGO=Ring,Tree",
"NCCL_PROTO=Simple",
"NCCL_MIN_NCHANNELS=4",
"NCCL_TUNER_PLUGIN=libnccl-tuner.so",
"NCCL_DYNAMIC_CHUNK_SIZE=524288",
"NCCL_P2P_NET_CHUNKSIZE=524288",
"NCCL_P2P_PCI_CHUNKSIZE=524288",
"NCCL_P2P_NVL_CHUNKSIZE=1048576",
"NCCL_FASTRAK_NUM_FLOWS=2",
"NCCL_FASTRAK_USE_SNAP=1",
"NCCL_FASTRAK_ENABLE_CONTROL_CHANNEL=0",
"NCCL_BUFFSIZE=8388608",
"CUDA_VISIBLE_DEVICES=0,1,2,3,4,5,6,7",
"NCCL_NET_GDR_LEVEL=PIX",
"NCCL_FASTRAK_ENABLE_HOTPATH_LOGGING=0",
"NCCL_FASTRAK_USE_LLCM=1",
"NCCL_NVLS_ENABLE=0"
```
#### What is new in this release:
* Releases tuner plugin for algorithm tuning. Please specify `NCCL_TUNER_PLUGIN=libnccl-tuner.so` and `NCCL_TUNER_CONFIG_PATH=/usr/local/nvidia/lib64/a3plus_tuner_config.textproto` to enable the tuner plugin; to opt-out, please specify `NCCL_TUNER_PLUGIN=UNUSED`.
* Releases NCCL and NCCL net plugin built with Cuda `12.2`.
* Releases NCCL core with NCCL `2.21`.
* Release `NCCL_FASTRAK_DUMP_COMM_STATS` to control the stats dump upon communicator teardown, by default the comm stats will be printed, to opt-out, please set `NCCL_FASTRAK_DUMP_COMM_STATS=0`.
* Release various bug fixes.

## Apr 17, 2024
#### Recommended GKE versions:
```
For 1.28 >= 1.28.8-gke.1095000
For 1.29 >= 1.29.3-gke.1093000
```
### NCCL plugin installer image: 
```
us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpxo/nccl-plugin-gpudirecttcpx-dev:v1.0.0
```
#### TCPXO-daemon image: 
```
us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpxo/tcpgpudmarxd-dev:v1.0.6-sctp
```
#### NCCL configs:
```
LD_LIBRARY_PATH=\"${LD_LIBRARY_PATH}:/usr/local/nvidia/lib64\"
NCCL_FASTRAK_CTRL_DEV=eth0
NCCL_FASTRAK_IFNAME=eth1,eth2,eth3,eth4,eth5,eth6,eth7,eth8
NCCL_SOCKET_IFNAME=eth0
NCCL_CROSS_NIC=0
NCCL_ALGO=Ring
NCCL_PROTO=Simple
NCCL_MIN_NCHANNELS=4
NCCL_DYNAMIC_CHUNK_SIZE=524288
NCCL_P2P_NET_CHUNKSIZE=524288
NCCL_P2P_PCI_CHUNKSIZE=524288
NCCL_P2P_NVL_CHUNKSIZE=1048576
NCCL_FASTRAK_NUM_FLOWS=2
NCCL_FASTRAK_USE_SNAP=1
NCCL_FASTRAK_ENABLE_CONTROL_CHANNEL=0
NCCL_BUFFSIZE=8388608
CUDA_VISIBLE_DEVICES=0,1,2,3,4,5,6,7
NCCL_NET_GDR_LEVEL=PIX
NCCL_FASTRAK_ENABLE_HOTPATH_LOGGING=0
NCCL_FASTRAK_USE_LLCM=1
```
