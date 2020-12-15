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
	"errors"
	"fmt"
	"github.com/stretchr/testify/assert"
	"io/ioutil"
	"os"
	"testing"
)

type pciDetailsGetterMock struct {
	mockBusID string
}

func (s *pciDetailsGetterMock) GetPciBusID(deviceID string) (string, error) {
	return s.mockBusID, nil
}

type pciDetailsGetterErrorMock struct {
	mockBusID string
}

func (s *pciDetailsGetterErrorMock) GetPciBusID(deviceID string) (string, error) {
	return "", errors.New("Failed to read pci bus id")
}

func Test_WhenFileIsGood_ReturnsContentsCorrectly(t *testing.T) {
	testSysNumaNodeGetter(t, "1\n", 1, false)
}

func Test_WhenFileIsMissing_ReturnsError(t *testing.T) {
	testSysNumaNodeGetter(t, "", -1, true)
}

func Test_WhenFileIsCorrupt_ReturnsError(t *testing.T) {
	testSysNumaNodeGetter(t, "nonsense", -1, true)
}

func Test_WhenFailsToGetPciBusId_ReturnsError(t *testing.T) {
	as := assert.New(t)

	mockPci := pciDetailsGetterErrorMock{mockBusID: ""}
	sut := NewSysNumaNodeGetter("a", &mockPci)

	numaNode, err := sut.Get("/dev/nvidia4")

	as.Equal(-1, numaNode)
	as.NotNil(err)
}

func testSysNumaNodeGetter(t *testing.T, numaNodeFileContents string, expectedResult int, expectError bool) {
	as := assert.New(t)

	wd, err := os.Getwd()
	testSysDir, err := ioutil.TempDir(wd, "sys")
	defer os.RemoveAll(testSysDir)

	mockPci := pciDetailsGetterMock{mockBusID: "00000000_00_09.0"}
	sut := NewSysNumaNodeGetter(testSysDir, &mockPci)

	dirname := fmt.Sprintf("%s/bus/pci/devices/0000_00_09.0", testSysDir)
	as.Nil(os.MkdirAll(dirname, 0644))
	filename := fmt.Sprintf("%s/numa_node", dirname)
	if numaNodeFileContents != "" {
		as.Nil(ioutil.WriteFile(filename, []byte(numaNodeFileContents), 0644))
	}

	numaNode, err := sut.Get("/dev/nvidia4")

	as.Equal(expectedResult, numaNode)
	if expectError {
		as.NotNil(err)
	} else {
		as.Nil(err)
	}
}
