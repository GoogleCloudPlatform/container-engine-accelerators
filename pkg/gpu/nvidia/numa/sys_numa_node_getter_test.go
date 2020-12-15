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
	"os"
	"syscall"
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

type mockFileSystem struct {
	files map[string][]byte
}

func (fs mockFileSystem) ReadFile(filename string) ([]byte, error) {
	contents, exists := fs.files[filename]
	if !exists {
		return nil, &os.PathError{Op: "open", Path: filename, Err: syscall.Errno(2)}
	}
	return contents, nil
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

	testSysDir := "/sys"
	filename := fmt.Sprintf("%s/bus/pci/devices/0000_00_09.0/numa_node", testSysDir)

	files := make(map[string][]byte)
	if numaNodeFileContents != "" {
		files[filename] = []byte(numaNodeFileContents)
	}

	mockPci := pciDetailsGetterMock{mockBusID: "00000000_00_09.0"}

	sut := newSysNumaNodeGetterMockableFileSystem(testSysDir, &mockPci, mockFileSystem{files: files})

	numaNode, err := sut.Get("/dev/nvidia4")

	as.Equal(expectedResult, numaNode)
	if expectError {
		as.NotNil(err)
	} else {
		as.Nil(err)
	}
}
