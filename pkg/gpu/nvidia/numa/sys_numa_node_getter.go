package numa

import (
	"fmt"
	"io/ioutil"
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia/pci"
	"github.com/golang/glog"
)

// NewSysNumaNodeGetter returns a NumaNodeGetter which maps device id to numa node by reading numa_node files under /sys.
func NewSysNumaNodeGetter(sysd string, pdg pci.PciDetailsGetter) NumaNodeGetter {
	return &sysNumaNodeGetter{sysDirectory: sysd, pciDetailsGetter: pdg}
}

// Gets NUMA node by looking under /sys
type sysNumaNodeGetter struct {
	sysDirectory     string // always /sys in production, but allow mocking for tests
	pciDetailsGetter pci.PciDetailsGetter
}

func (s *sysNumaNodeGetter) Get(deviceID string) (int, error) {
	pciBusID, err := s.pciDetailsGetter.GetPciBusID(deviceID)
	if err != nil {
		return -1, fmt.Errorf("Failed to get pci bus id for %s: %v", deviceID, err)
	}

	filename := fmt.Sprintf("%s/bus/pci/devices/%s/numa_node", s.sysDirectory, strings.ToLower(pciBusID[4:]))
	numaStr, err := ioutil.ReadFile(filename)
	if err != nil {
		return -1, fmt.Errorf("Failed to read file %s: %v", filename, err)
	}

	numa, err := strconv.ParseInt(strings.Trim(string(numaStr), "\n"), 10, 8)
	if err != nil {
		return -1, fmt.Errorf("Failed parse \"%s\" read from file %s: %v", numaStr, filename, err)
	}

	glog.Infof("Mapped device %s to pciBusID %s and NUMA node %d\n", deviceID, pciBusID, numa)

	return int(numa), nil
}
