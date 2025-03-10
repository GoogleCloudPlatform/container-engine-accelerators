# Best practice to run workload with GPUDirect-TCPX(O)
* [Automatic Sidecar Termination](./best-practice.md#automatic-sidecar-termination)
* [Enable LL128 for latency reduction](./best-practice.md#enable-ll128-for-latency-reduction)
* [Setup Startup Probe for the TCPXO-daemon Sidecar](./best-practice.md#setup-startup-probe-for-the-tcpxo-daemon-sidecar)
* [Topology awareness scheduling](./best-practice.md#topology-awareness-scheduling)

## Automatic Sidecar Termination
The current TCPX(O) setup requires both the tcpxo-daemon and main application run in the same Pod, because these two containers need to use the same network namespace when `hostNetwork:false`. As the example in https://cloud.google.com/kubernetes-engine/docs/how-to/gpu-bandwidth-gpudirect-tcpx shows, we commonly put them as containers under the same Pod. The problem with this workaround is the tcpxo-daemon doesn’t keep the same lifecycle as the main application container. The tcpxo-daemon will keep running even if the main application container completes the work, blocking the Job/Pod to complete.

We leverage the [Sidecar Containers](https://kubernetes.io/docs/concepts/workloads/pods/sidecar-containers/) feature in kubernetes to resolve this problem. This feature is by default enabled in **GKE versions minor version 1.29 and later**.

<table>
<tr>
<th> Before </th>
<th> After </th>
</tr>
<tr>
<td>

```
apiVersion: v1
kind: Pod
metadata:
  name: a3-mega-workloads
  annotations:
...
  containers:
    - name: tcpxo-daemon
    - name: main-application-container
  volumes:
....

```

</td>
<td>

```
apiVersion: v1
kind: Pod
metadata:
  name: a3-mega-workloads
  annotations:
  initContainers:
    - name: tcpxo-daemon
      restartPolicy: Always
  containers:
    - name: main-application-container
  volumes:
....

```

</td>
</tr>
</table>

Using the sidecar containers feature, the tcpxo-daemon is guaranteed to auto-terminate after the main application container completes. Reference: https://kccnceu2024.sched.com/event/1YeS0

## Enable LL128 for Latency Reduction
LL128 is a NCCL feature that gives non-trivial latency reductions for small-medium msg sizes.

You could update the following NCCL configs in your workload to enable LL128: 
- Ensure the NCCL plugin installer image is in release >= Feb 06, 2025 in [GPUDirect-TCPXO release notes](github.com/GoogleCloudPlatform/container-engine-accelerators/gpudirect-tcpxo/README.md), with this version or later:
```
us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpxo/nccl-plugin-gpudirecttcpx-
dev:v1.0.8-1
```
- Set the following environment variable in your workload manifest:
```
NCCL_LIB_DIR="/usr/local/nvidia/lib64
```
- Configure your workload to execute the `nccl-env-profile-ll128.sh` script when the container starts. Set the following command in your workload manifest:
```
source ${NCCL_LIB_DIR}/nccl-env-profile-ll128.sh
``` 
The `nccl-env-profile-ll128.sh` script has different values for the following environment variables compared to the `n`ccl-env-profile.sh` script:
```
NCCL_PROTO=Simple,LL128
NCCL_TUNER_CONFIG_PATH=/usr/local/nvidia/lib64/a3plus_tuner_config_ll128.textproto
NCCL_SHIMNET_GUEST_CONFIG_CHECKER_CONFIG_FILE=/usr/local/nvidia/lib64/a3plus_guest_config_ll128.textproto
```
## Setup Startup Probe for the TCPXO-daemon Sidecar
When TCPXO-daemon sidecar starts, it will execute an initialization process. If the container lands on a bad node(with host component issues), the TCPXO-daemon will not continue to work. Setup a [startup probe](https://kubernetes.io/docs/concepts/configuration/liveness-readiness-startup-probes/#startup-probe) to fail the container earlier with such cases.

- Add `HEALTH_CHECK_LOG_FILE` as an environment variable into your tcpxo-daemon container manifest. You can specify any file name you want as the value for this key, e.g. `/run/health-check`. Noted: the file name can't include a low line(e.g. `/run/health_check`).
- If `HEALTH_CHECK_LOG_FILE` presents, once the TCPXO-daemon finishes the initialization (typically takes ~ 30s), it will create the corresponding file and add log `Buffer manager initialization completed`. Unsuccessful initialization will not create any file. 
- An example of startup probe:
  ```
      - name: tcpxo-daemon
      image: us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpxo/tcpgpudmarxd-dev:v1.0.8
      imagePullPolicy: Always
      command: ["/bin/sh", "-c"]
      args:
        - |
          set -ex
          chmod 755 /fts/entrypoint_rxdm_container.sh
          /fts/entrypoint_rxdm_container.sh --num_hops=2 --num_nics=8 --uid= --alsologtostderr
      volumeMounts:
        - name: nvidia-install-dir-host
          mountPath: /usr/local/nvidia
      env:
        - name: LD_LIBRARY_PATH
          value: /usr/local/nvidia/lib64
        - name: HEALTH_CHECK_LOG_FILE
          value: /run/health-check
      startupProbe:
        initialDelaySeconds: 1
        periodSeconds: 5
        timeoutSeconds: 1
        successThreshold: 1
        failureThreshold: 6
        exec:
          command:
            - cat
            - /run/health-check
  ```

## Topology Awareness Scheduling
If you are using the compact placement when creating A3+ nodepool, you can set up topology awareness configuration to gain better network performance.

Please check [gke-topology-scheduler](https://github.com/GoogleCloudPlatform/container-engine-accelerators/tree/master/gke-topology-scheduler) on the external github for an example of how to take advantage of the performance boosts of this feature.

In our internal tests, setting topology awareness scheduling improves the performance by ~10% for 128 nodes all-gather test with  message size above 1MB.
