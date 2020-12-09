package pci

import (
	"fmt"
	"strings"
	"github.com/golang/glog"
	"github.com/NVIDIA/gpu-monitoring-tools/bindings/go/nvml"
)

// NewNvmlPciDetailsGetter returns a PciDetailsGetter that uses Nvidia's NVML library to map device id to PCI bus id.
func NewNvmlPciDetailsGetter() (PciDetailsGetter, error) {
	err := nvml.Init()
	if err != nil {
		return fmt.Errorf("Failed to initialize nvml: %v", err)
	}

	numDevices, err := nvml.GetDeviceCount()
	if err != nil {
		return nil, fmt.Errorf("Failed to get device count: %v", err)
	}
	glog.Infof("Found %d GPUs", numDevices)

	deviceIDToBusID := make(map[string]string)
	for deviceIndex := uint(0); deviceIndex < numDevices; deviceIndex++ {
		device, err := nvml.NewDevice(deviceIndex)
		if err != nil {
			return nil, fmt.Errorf("Failed to read device with index %d: %v", deviceIndex, err)
		}
		deviceID := strings.Replace(device.Path, "/dev/", "", 1)
		pciBusID := device.PCI.BusID
		glog.Infof("Mapped GPU %s to PCI bus id %s", deviceID, pciBusID)
		deviceIDToBusID[deviceID] = pciBusID
	}
	return &nvmlPciDetailsGetter{deviceIDToBusID: deviceIDToBusID}, nil
}

type nvmlPciDetailsGetter struct {
	deviceIDToBusID map[string]string
}

func (dg *nvmlPciDetailsGetter) GetPciBusID(deviceID string) (string, error) {
	busID, exists := dg.deviceIDToBusID[deviceID]
	if !exists {
		return "", fmt.Errorf("Could not find GPU \"%s\"", deviceID)
	}
	return busID, nil
}
