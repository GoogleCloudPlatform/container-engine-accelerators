// Copyright 2020 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package numa

import (
	"fmt"
	"io/ioutil"
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia/pci"
	"github.com/golang/glog"
)

type fileSystem interface {
	ReadFile(filename string) ([]byte, error)
}

type realFileSystem struct{}

func (realFileSystem) ReadFile(filename string) ([]byte, error) {
	return ioutil.ReadFile(filename)
}

// NewSysNumaNodeGetter returns a NumaNodeGetter which maps device id to numa node by reading numa_node files under /sys.
func NewSysNumaNodeGetter(sysd string, pdg pci.PciDetailsGetter) NumaNodeGetter {
	return &sysNumaNodeGetter{sysDirectory: sysd, pciDetailsGetter: pdg, fs: realFileSystem{}}
}

// newSysNumaNodeGetterMockableFileSystem returns a NumaNodeGetter (like NewSysNumaNodeGetter) but allowing injection of a mock filesystem for testing.
func newSysNumaNodeGetterMockableFileSystem(sysd string, pdg pci.PciDetailsGetter, fs fileSystem) NumaNodeGetter {
	return &sysNumaNodeGetter{sysDirectory: sysd, pciDetailsGetter: pdg, fs: fs}
}

// Gets NUMA node by looking under /sys
type sysNumaNodeGetter struct {
	sysDirectory     string
	pciDetailsGetter pci.PciDetailsGetter
	fs               fileSystem
}

func (s *sysNumaNodeGetter) Get(deviceID string) (int, error) {
	pciBusID, err := s.pciDetailsGetter.GetPciBusID(deviceID)
	if err != nil {
		return -1, fmt.Errorf("Failed to get pci bus id for %s: %v", deviceID, err)
	}

	filename := fmt.Sprintf("%s/bus/pci/devices/%s/numa_node", s.sysDirectory, strings.ToLower(pciBusID[4:]))
	numaStr, err := s.fs.ReadFile(filename)
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
