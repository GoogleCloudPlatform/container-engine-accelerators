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
On nodes that have NRI enabled, run:
```
export IMAGE="your-device-injector-image-path"

kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: device-injector
  namespace: kube-system
  labels:
    k8s-app: device-injector
spec:
  selector:
    matchLabels:
      k8s-app: device-injector
  updateStrategy:
    type: RollingUpdate
  template:
    metadata:
      labels:
        name: device-injector
        k8s-app: device-injector
    spec:
      priorityClassName: system-node-critical
      tolerations:
        - operator: "Exists"
      hostNetwork: true
      containers:
        - image: ${IMAGE}
          name: device-injector
          resources:
            requests:
              cpu: 150m
          securityContext:
            privileged: true
          volumeMounts:
            - name: root
              mountPath: /host
            - name: nri
              mountPath: /var/run/nri
      volumes:
        - name: root
          hostPath:
            path: /
        - name: nri
          hostPath:
            path: /var/run/nri
EOF
```
