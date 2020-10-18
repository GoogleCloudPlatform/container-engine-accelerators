package metrics

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
}

// Stop performs cleanup operations and stops the metric server.
func (m *MetricServer) Stop() {
}
