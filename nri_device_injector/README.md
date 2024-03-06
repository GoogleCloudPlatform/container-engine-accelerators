# NRI Device Injector Plugin

Based on [containerd NRI](https://github.com/containerd/nri/tree/main) and the [example plugin](https://github.com/containerd/nri/tree/main/plugins/device-injector), this device injector plugin injects devices into containers specified by pod annotations. 

## To annotate pod with device injections
Devices are annotated with the key `devices.gke.io/container.$CONTAINER_NAME` to have devices injected into `$CONTAINER_NAME`. Full annotation of a device is 
```
annotations:
    devices.gke.io/container.$CONTAINER_NAME: |+
        - path: $PATH
          type: $TYPE
          major: $MAJOR
          minor: $MINOR
          file_mode: $FILE_MODE
          uid: $UID
          gid: $GID
```
`path` is madatory, and the rest can be omitted.
Example annotation to inject 3 GPU devices into container `test`:
```
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