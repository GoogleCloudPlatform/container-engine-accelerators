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
	mockBusId string
}

func (s *pciDetailsGetterMock) GetPciBusId(deviceId string) (string, error) {
	return s.mockBusId, nil
}

type pciDetailsGetterErrorMock struct {
	mockBusId string
}

func (s *pciDetailsGetterErrorMock) GetPciBusId(deviceId string) (string, error) {
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

	mockPci := pciDetailsGetterErrorMock{mockBusId: ""}
	sut := NewSysNumaNodeGetter("a", &mockPci)

	numaNode, err := sut.Get("/dev/nvidia4")

	as.Equal(-1, numaNode)
	as.NotNil(err)
}

func testSysNumaNodeGetter(t *testing.T, numaNodeFileContents string, expectedResult int, expectError bool) {
	as := assert.New(t)

	testSysDir, err := ioutil.TempDir("", "sys")
	defer os.RemoveAll(testSysDir)

	mockPci := pciDetailsGetterMock{mockBusId: "0000_00_09.0"}
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
