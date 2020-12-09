package numa

// NumaNodeGetter maps a device id (such as nvidia0) to a NUMA node.
type NumaNodeGetter interface {
	Get(deviceID string) (int, error)
}
