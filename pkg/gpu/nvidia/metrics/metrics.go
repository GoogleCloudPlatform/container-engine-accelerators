// Copyright 2017 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package metrics

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/NVIDIA/gpu-monitoring-tools/bindings/go/nvml"
	"github.com/golang/glog"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type device interface{}
type deviceStatus interface{}

type metricsCollector interface {
	collectGPUDevice(deviceName string) (*nvml.Device, error)
	collectGPUDeviceIndex(deviceName string) (uint, error)
	collectStatus(*nvml.Device) (status *nvml.DeviceStatus, err error)
	collectDutyCycle(string, time.Duration) (uint, error)
}

var gmc metricsCollector

type mCollector struct{}

func (t *mCollector) collectGPUDevice(deviceName string) (*nvml.Device, error) {
	return DeviceFromName(deviceName)
}

func (t *mCollector) collectGPUDeviceIndex(deviceName string) (uint, error) {
	return DeviceIndexFromName(deviceName)
}

func (t *mCollector) collectStatus(d *nvml.Device) (status *nvml.DeviceStatus, err error) {
	status, err = d.Status()
	return status, err
}

func (t *mCollector) collectDutyCycle(uuid string, since time.Duration) (uint, error) {
	return AverageGPUUtilization(uuid, since)
}

var (
	// DutyCycleNodeGpu reports the percent of time when the GPU was actively processing per Node.
	DutyCycleNodeGpu = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "duty_cycle_gpu_node",
			Help: "Percent of time when the GPU was actively processing",
		},
		[]string{"node_name", "make", "accelerator_index", "model"})

	// MemoryTotalNodeGpu reports the total memory available on the GPU per Node.
	MemoryTotalNodeGpu = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "memory_total_gpu_node",
			Help: "Total memory available on the GPU in bytes",
		},
		[]string{"node_name", "make", "accelerator_index", "model"})

	// MemoryUsedNodeGpu reports GPU memory allocated per Node.
	MemoryUsedNodeGpu = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "memory_used_gpu_node",
			Help: "Allocated GPU memory in bytes",
		},
		[]string{"node_name", "make", "accelerator_index", "model"})

	// DutyCycle reports the percent of time when the GPU was actively processing per container.
	DutyCycle = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "duty_cycle",
			Help: "Percent of time when the GPU was actively processing",
		},
		[]string{"namespace", "pod", "container", "make", "accelerator_id", "model"})

	// MemoryTotal reports the total memory available on the GPUper container.
	MemoryTotal = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "memory_total",
			Help: "Total memory available on the GPU in bytes",
		},
		[]string{"namespace", "pod", "container", "make", "accelerator_id", "model"})

	// MemoryUsed reports GPU memory allocated per container.
	MemoryUsed = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "memory_used",
			Help: "Allocated GPU memory in bytes",
		},
		[]string{"namespace", "pod", "container", "make", "accelerator_id", "model"})

	// AcceleratorRequests reports the number of GPU devices requested by the container.
	AcceleratorRequests = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "request",
			Help: "Number of accelerator devices requested by the container",
		},
		[]string{"namespace", "pod", "container", "resource_name"})
)

const metricsResetInterval = time.Minute

// MetricServer exposes GPU metrics for all containers and nodes in prometheus format on the specified port.
type MetricServer struct {
	collectionInterval   int
	port                 int
	metricsEndpointPath  string
	lastMetricsResetTime time.Time
}

func NewMetricServer(collectionInterval, port int, metricsEndpointPath string) *MetricServer {
	return &MetricServer{
		collectionInterval:   collectionInterval,
		port:                 port,
		metricsEndpointPath:  metricsEndpointPath,
		lastMetricsResetTime: time.Now(),
	}
}

// Start performs necessary initializations and starts the metric server.
func (m *MetricServer) Start() error {
	glog.Infoln("Starting metrics server")

	driverVersion, err := nvml.GetDriverVersion()
	if err != nil {
		return fmt.Errorf("failed to query nvml: %v", err)
	}
	glog.Infof("nvml initialized successfully. Driver version: %s", driverVersion)

	err = DiscoverGPUDevices()
	if err != nil {
		return fmt.Errorf("failed to discover GPU devices: %v", err)
	}

	go func() {
		http.Handle(m.metricsEndpointPath, promhttp.Handler())
		err := http.ListenAndServe(fmt.Sprintf(":%d", m.port), nil)
		if err != nil {
			glog.Infof("Failed to start metric server: %v", err)
		}
	}()

	go m.collectMetrics()
	return nil
}

func (m *MetricServer) collectMetrics() {
	gmc = &mCollector{}
	t := time.NewTicker(time.Millisecond * time.Duration(m.collectionInterval))
	defer t.Stop()

	for {
		select {
		case <-t.C:
			devices, err := GetDevicesForAllContainers()
			if err != nil {
				glog.Errorf("Failed to get devices for containers: %v", err)
				continue
			}
			gpuDevices := GetAllGpuDevices()
			m.updateMetrics(devices, gpuDevices)
		}
	}
}

func getGpuMetrics(device string, d *nvml.Device) (uint, uint64, error) {
	status, err := gmc.collectStatus(d)
	if err != nil {
		glog.Errorf("Failed to get device status for %s: %v", device, err)
	}
	mem := status.Memory
	dutyCycle, err := gmc.collectDutyCycle(d.UUID, time.Second*10)
	if err != nil {
		return 0, 0, fmt.Errorf("Failed to get dutyCycle: %v", err)
	}
	return dutyCycle, *mem.Global.Used, nil
}

func (m *MetricServer) updateMetrics(containerDevices map[ContainerID][]string, gpuDevices map[string]*nvml.Device) {
	m.resetMetricsIfNeeded()

	for container, devices := range containerDevices {
		AcceleratorRequests.WithLabelValues(container.namespace, container.pod, container.container, gpuResourceName).Set(float64(len(devices)))

		for _, device := range devices {
			d, err := gmc.collectGPUDevice(device)
			if err != nil {
				glog.Errorf("Failed to get device for %s: %v", device, err)
				continue
			}
			dutyCycle, usedMemory, err := getGpuMetrics(device, d)
			if err != nil {
				glog.Infof("Error calculating duty cycle for device: %s: %v. Skipping this device", device, err)
				continue
			}

			DutyCycle.WithLabelValues(container.namespace, container.pod, container.container, "nvidia", d.UUID, *d.Model).Set(float64(dutyCycle))
			MemoryTotal.WithLabelValues(container.namespace, container.pod, container.container, "nvidia", d.UUID, *d.Model).Set(float64(*d.Memory) * 1024 * 1024) // memory reported in bytes
			MemoryUsed.WithLabelValues(container.namespace, container.pod, container.container, "nvidia", d.UUID, *d.Model).Set(float64(usedMemory) * 1024 * 1024) // memory reported in bytes
		}
	}
	nodeName := os.Getenv("NODE_NAME")
	for device, d := range gpuDevices {
		accel_index, err := gmc.collectGPUDeviceIndex(device)
		if err != nil {
			glog.Infof("Error finding device index for device: %s: %v. Skipping this device", device, err)
			continue
		}
		accel_index_str := strconv.FormatUint(uint64(accel_index), 10)
		dutyCycle, usedMemory, err := getGpuMetrics(device, d)
		if err != nil {
			glog.Infof("Error calculating duty cycle for device: %s: %v. Skipping this device", device, err)
			continue
		}
		DutyCycleNodeGpu.WithLabelValues(nodeName, "nvidia", accel_index_str, *d.Model).Set(float64(dutyCycle))
		MemoryTotalNodeGpu.WithLabelValues(nodeName, "nvidia", accel_index_str, *d.Model).Set(float64(*d.Memory) * 1024 * 1024) // memory reported in bytes
		MemoryUsedNodeGpu.WithLabelValues(nodeName, "nvidia", accel_index_str, *d.Model).Set(float64(usedMemory) * 1024 * 1024) // memory reported in bytes
	}
}

func (m *MetricServer) resetMetricsIfNeeded() {
	if time.Now().After(m.lastMetricsResetTime.Add(metricsResetInterval)) {
		AcceleratorRequests.Reset()
		DutyCycle.Reset()
		MemoryTotal.Reset()
		MemoryUsed.Reset()
		DutyCycleNodeGpu.Reset()
		MemoryTotalNodeGpu.Reset()
		MemoryUsedNodeGpu.Reset()

		m.lastMetricsResetTime = time.Now()
	}
}

// Stop performs cleanup operations and stops the metric server.
func (m *MetricServer) Stop() {
}
