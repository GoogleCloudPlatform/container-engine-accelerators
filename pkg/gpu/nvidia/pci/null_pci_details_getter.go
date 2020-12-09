package pci

import "C"
import (
	"errors"
)

func NewNullPciDetailsGetter() PciDetailsGetter {
	return &nullPciDetailsGetter{}
}

type nullPciDetailsGetter struct {
}

func (s *nullPciDetailsGetter) GetPciBusId(deviceId string) (string, error) {
	return "", errors.New("PciDetailsGetter nulled out")
}
