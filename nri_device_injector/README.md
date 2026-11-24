# NRI Device Injector Plugin

Based on [containerd NRI](https://github.com/containerd/nri/tree/main) and the [example plugin](https://github.com/containerd/nri/tree/main/plugins/device-injector), this device injector plugin injects allowlisted NVIDIA GPU and GPUDirect character devices into containers specified by pod annotations.

## Flags

- `--allowed-namespaces`: Comma-separated namespaces whose pods may use `devices.gke.io/container.*` annotations. Empty (`""`) rejects every annotation, and `"*"` allows all namespaces (the Standard cluster manifest sets `--allowed-namespaces=default`).
- `--allow-mig-capabilities`: Allow injecting `/dev/nvidia-caps/nvidia-cap[0-9]+` devices from pod annotations (default `false`).

## To annotate pod with device injections
Devices are annotated with the key `devices.gke.io/container.$CONTAINER_NAME` to have devices injected into `$CONTAINER_NAME`:
```yaml
annotations:
    devices.gke.io/container.$CONTAINER_NAME: |+
        - path: $PATH
```
Only `path` is accepted. Device major/minor numbers, permissions, UID, and GID are read directly from the host device node via `lstat`. Permitted character device paths are:
- `/dev/nvidia[0-9]+`
- `/dev/nvidiactl`
- `/dev/nvidia-uvm`
- `/dev/dmabuf_import_helper`
- `/dev/nvidia-caps/nvidia-cap[0-9]+` (only when `--allow-mig-capabilities` is enabled)

Example annotation to inject 3 GPU devices into container `test`:
```yaml
annotations:
    devices.gke.io/container.test: |+
        - path: /dev/nvidia0
        - path: /dev/nvidia1
        - path: /dev/nvidia2
```

## To deploy device injector plugin in GKE cluster
### Build device injector plugin image
From root of the repository, run:
  `docker build -f nri_device_injector/Dockerfile .`
### Apply device injector manifest
  `kubectl apply -f nri-device-injector.yaml`
The device injector enables NRI on the node and runs the NRI device injector plugin.
