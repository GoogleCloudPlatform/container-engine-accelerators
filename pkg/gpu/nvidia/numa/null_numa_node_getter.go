package numa

import (
	"errors"
)

// NewNullNumaNodeGetter returns a NumaNodeGetter which always fails, for use when NUMA TopologyInfo is disabled.
func NewNullNumaNodeGetter() NumaNodeGetter {
	return &nullNumaNodeGetter{}
}

type nullNumaNodeGetter struct {
}

func (s *nullNumaNodeGetter) Get(deviceID string) (int, error) {
	return -1, errors.New("Topology info disabled, won't try and get NUMA node")
}
