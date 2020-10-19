package metrics

import (
	"fmt"
	"net/http"

	"github.com/golang/glog"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	// DutyCycle reports the percent of time when the GPU was actively processing.
	DutyCycle = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "duty_cycle",
			Help: "Percent of time when the GPU was actively processing",
		},
		[]string{"namespace", "pod", "container", "make", "accelerator_id", "model"})

	// MemoryTotal reports the total memory available on the GPU.
	MemoryTotal = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "memory_total",
			Help: "Total memory available on the GPU in bytes",
		},
		[]string{"namespace", "pod", "container", "make", "accelerator_id", "model"})

	// MemoryUsed reports GPU memory allocated.
	MemoryUsed = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "memory_used",
			Help: "Allocated GPU memory in bytes",
		},
		[]string{"namespace", "pod", "container", "make", "accelerator_id", "model"})

	// AcceleratorRequests reports the number of GPU devices requested by the container.
	AcceleratorRequests = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "accelerator_requests",
			Help: "Number of accelerator devices requested by the container",
		},
		[]string{"resource_name"})
)

// MetricServer exposes GPU metrics for all containers in prometheus format on the specified port.
type MetricServer struct {
	collectionInterval  int
	port                int
	metricsEndpointPath string
}

func NewMetricServer(collectionInterval, port int, metricsEndpointPath string) *MetricServer {
	return &MetricServer{
		collectionInterval:  collectionInterval,
		port:                port,
		metricsEndpointPath: metricsEndpointPath,
	}
}

// Start performs necessary initializations and starts the metric server.
func (m *MetricServer) Start() {
	glog.Infoln("Starting metrics server")
	go func() {
		http.Handle(m.metricsEndpointPath, promhttp.Handler())
		err := http.ListenAndServe(fmt.Sprintf(":%d", m.port), nil)
		if err != nil {
			glog.Infof("Failed to start metric server: %v", err)
		}
	}()
}

// Stop performs cleanup operations and stops the metric server.
func (m *MetricServer) Stop() {
}
