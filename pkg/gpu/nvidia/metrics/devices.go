package metrics

import (
	"context"
	"fmt"
	"net"
	"time"

	"google.golang.org/grpc"
	podresources "k8s.io/kubernetes/pkg/kubelet/apis/podresources/v1alpha1"
)

var (
	socketPath      = "/var/lib/kubelet/pod-resources/kubelet.sock"
	gpuResourceName = "nvidia.com/gpu"

	connectionTimeout = 10 * time.Second
)

// ContainerID uniquely identifies a container.
type ContainerID struct {
	namespace string
	pod       string
	container string
}

// GetDevicesForAllContainers returns a map with container as the key and the list of devices allocated to that container as the value.
func GetDevicesForAllContainers() (map[ContainerID][]string, error) {
	containerDevices := make(map[ContainerID][]string)
	conn, err := grpc.Dial(
		socketPath,
		grpc.WithInsecure(),
		grpc.WithDialer(func(addr string, timeout time.Duration) (net.Conn, error) {
			return net.DialTimeout("unix", addr, timeout)
		}))
	if err != nil {
		return containerDevices, fmt.Errorf("error connecting to kubelet PodResourceLister service: %v", err)
	}
	client := podresources.NewPodResourcesListerClient(conn)

	resp, err := client.List(context.Background(), &podresources.ListPodResourcesRequest{})
	if err != nil {
		return containerDevices, fmt.Errorf("error listing pod resources: %v", err)
	}

	for _, pod := range resp.PodResources {
		container := ContainerID{
			namespace: pod.Namespace,
			pod:       pod.Name,
		}

		for _, c := range pod.Containers {
			container.container = c.Name
			for _, d := range c.Devices {
				if len(d.DeviceIds) == 0 || d.ResourceName != gpuResourceName {
					continue
				}
				containerDevices[container] = make([]string, 0)
				for _, deviceID := range d.DeviceIds {
					containerDevices[container] = append(containerDevices[container], deviceID)
				}
			}
		}
	}

	return containerDevices, nil
}
