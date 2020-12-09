package numa

import (
	"fmt"
	"io/ioutil"
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia/pci"
	"github.com/golang/glog"
)

func NewSysNumaNodeGetter(sysd string, pdg pci.PciDetailsGetter) NumaNodeGetter {
	return &sysNumaNodeGetter{sysDirectory: sysd, pciDetailsGetter: pdg}
}

// Gets NUMA node by looking under /sys
type sysNumaNodeGetter struct {
	sysDirectory     string // always /sys in production, but allow mocking for tests
	pciDetailsGetter pci.PciDetailsGetter
}

func (s *sysNumaNodeGetter) Get(deviceId string) (int, error) {
	pciBusId, err := s.pciDetailsGetter.GetPciBusId(deviceId)
	if err != nil {
		return -1, fmt.Errorf("Failed to get pci bus id for %s: %v", deviceId, err)
	}

	filename := fmt.Sprintf("%s/bus/pci/devices/%s/numa_node", s.sysDirectory, strings.ToLower(pciBusId))
	numaStr, err := ioutil.ReadFile(filename)
	if err != nil {
		return -1, fmt.Errorf("Failed to read file %s: %v", filename, err)
	}

	numa, err := strconv.ParseInt(strings.Trim(string(numaStr), "\n"), 10, 8)
	if err != nil {
		return -1, fmt.Errorf("Failed parse \"%s\" read from file %s: %v", numaStr, filename, err)
	}

	glog.Infof("Mapped device %s to PciBusId %s and NUMA node %d\n", deviceId, pciBusId, numa)

	return int(numa), nil
}
