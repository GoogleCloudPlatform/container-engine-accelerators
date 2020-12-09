package pci

import (
	"fmt"
	"strings"
	"github.com/golang/glog"
	"github.com/NVIDIA/gpu-monitoring-tools/bindings/go/nvml"
)

func NewNvmlPciDetailsGetter() (PciDetailsGetter, error) {

	numDevices, err := nvml.GetDeviceCount()
	if err != nil {
		return nil, fmt.Errorf("Failed to get device count: %v", err)
	}
	glog.Infof("Found %d GPUs", numDevices)

	deviceIdToBusId := make(map[string]string)   
	for deviceIndex := uint(0); deviceIndex < numDevices; deviceIndex++ {
		device, err := nvml.NewDevice(deviceIndex)
		if err != nil {
			return nil, fmt.Errorf("Failed to read device with index %d: %v", deviceIndex, err)
		}
		deviceId := strings.Replace(device.Path, "/dev/", "", 1)
		pciBusId := device.PCI.BusID
		glog.Infof("Mapped GPU %s to PCI bus id %s", deviceId, pciBusId)        
		deviceIdToBusId[deviceId] = pciBusId
	}
	return &nvmlPciDetailsGetter{deviceIdToBusId: deviceIdToBusId}, nil
}

type nvmlPciDetailsGetter struct {
	deviceIdToBusId map[string]string
}

func (dg *nvmlPciDetailsGetter) GetPciBusId(deviceId string) (string, error) {
	busId, exists := dg.deviceIdToBusId[deviceId]
	if !exists {
		return "", fmt.Errorf("Could not find GPU \"%s\"", deviceId)
	}
	return busId, nil
}
