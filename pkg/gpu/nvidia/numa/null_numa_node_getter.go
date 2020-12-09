package numa

import (
	"errors"
)

func NewNullNumaNodeGetter() NumaNodeGetter {
	return &nullNumaNodeGetter{}
}

type nullNumaNodeGetter struct {
}

func (s *nullNumaNodeGetter) Get(deviceId string) (int, error) {
	return -1, errors.New("Topology info disabled, won't try and get NUMA node")
}
