// Copyright 2026 Google Inc. All Rights Reserved.
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

package reset

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia/gpusharing"
)

type fakeMIGDevices struct {
	mu           sync.Mutex
	partitionIDs map[string][]string
	discovered   []string
	discoverErr  map[string]error
	onDiscover   func(gpu, partitionSize string) error
}

func (f *fakeMIGDevices) PartitionIDs(gpu string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.partitionIDs[gpu]...)
}

func (f *fakeMIGDevices) DiscoverGPU(gpu, partitionSize string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.onDiscover != nil {
		if err := f.onDiscover(gpu, partitionSize); err != nil {
			return err
		}
	}
	if err, ok := f.discoverErr[gpu]; ok && err != nil {
		return err
	}
	f.discovered = append(f.discovered, fmt.Sprintf("%s:%s", gpu, partitionSize))
	return nil
}

type fakeModeNVML struct {
	busIDs map[PhysicalGPU]string
	busErr map[PhysicalGPU]error
}

func defaultFakeModeNVML() *fakeModeNVML {
	return &fakeModeNVML{
		busIDs: map[PhysicalGPU]string{
			"nvidia0": "00000000:00:04.0",
			"nvidia1": "00000000:00:05.0",
			"nvidia2": "00000000:85:00.0",
		},
	}
}

func (f *fakeModeNVML) Shutdown() error                         { return nil }
func (f *fakeModeNVML) Init() error                             { return nil }
func (f *fakeModeNVML) Processes(_ PhysicalGPU) ([]int, error)  { return nil, nil }
func (f *fakeModeNVML) FabricReady(_ PhysicalGPU) (bool, error) { return true, nil }
func (f *fakeModeNVML) Persistence(_ PhysicalGPU) (bool, error) { return true, nil }
func (f *fakeModeNVML) SetPersistence(_ PhysicalGPU, _ bool) error {
	return nil
}
func (f *fakeModeNVML) IsAmpere(_ PhysicalGPU) (bool, error) { return false, nil }
func (f *fakeModeNVML) PCIBusID(gpu PhysicalGPU) (string, error) {
	if f.busErr != nil {
		if err, ok := f.busErr[gpu]; ok {
			return "", err
		}
	}
	if f.busIDs != nil {
		if id, ok := f.busIDs[gpu]; ok {
			return id, nil
		}
	}
	return fmt.Sprintf("00000000:00:%02x.0", 4), nil
}

func newTestMode(cfg Config, migDevs MIGDevices, nv NVML, exec executor) Mode {
	m := NewMode(cfg, WithMIGDevices(migDevs), WithNVML(nv)).(*modeImpl)
	if exec != nil {
		m.run = exec
	}
	return m
}

func TestMode_DeviceIDs(t *testing.T) {
	migDevs := &fakeMIGDevices{
		partitionIDs: map[string][]string{
			"nvidia0": {"nvidia0/gi1", "nvidia0/gi2"},
		},
	}
	cases := []struct {
		name string
		cfg  Config
		mig  MIGDevices
		gpu  PhysicalGPU
		want []string
	}{
		{
			name: "standard",
			cfg:  Config{},
			gpu:  "nvidia0",
			want: []string{"nvidia0"},
		},
		{
			name: "time-sharing",
			cfg:  Config{MaxSharedClientsPerGPU: 3, Sharing: gpusharing.TimeSharing},
			gpu:  "nvidia2",
			want: []string{"nvidia2/vgpu0", "nvidia2/vgpu1", "nvidia2/vgpu2"},
		},
		{
			name: "mig",
			cfg:  Config{PartitionSize: "1g.5gb"},
			mig:  migDevs,
			gpu:  "nvidia0",
			want: []string{"nvidia0/gi1", "nvidia0/gi2"},
		},
		{
			name: "mig nil manager",
			cfg:  Config{PartitionSize: "1g.5gb"},
			mig:  nil,
			gpu:  "nvidia0",
			want: nil,
		},
		{
			name: "mig + sharing",
			cfg:  Config{PartitionSize: "1g.5gb", MaxSharedClientsPerGPU: 2, Sharing: gpusharing.MPS},
			mig:  migDevs,
			gpu:  "nvidia0",
			want: []string{"nvidia0/gi1/vgpu0", "nvidia0/gi1/vgpu1", "nvidia0/gi2/vgpu0", "nvidia0/gi2/vgpu1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMode(tc.cfg, WithMIGDevices(tc.mig), WithNVML(defaultFakeModeNVML()))
			if got := m.DeviceIDs(tc.gpu); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("DeviceIDs(%q) = %v, want %v", tc.gpu, got, tc.want)
			}
		})
	}
}

func TestMode_NoopPreResetAndConverge(t *testing.T) {
	for _, cfg := range []Config{{}, {MaxSharedClientsPerGPU: 4, Sharing: gpusharing.TimeSharing}} {
		m := NewMode(cfg, WithNVML(defaultFakeModeNVML()))
		if err := m.PreReset(context.Background(), []PhysicalGPU{"nvidia0", "nvidia1"}); err != nil {
			t.Fatalf("PreReset(%+v) = %v", cfg, err)
		}
		if err := m.Converge(context.Background(), []PhysicalGPU{"nvidia0", "nvidia1"}); err != nil {
			t.Fatalf("Converge(%+v) = %v", cfg, err)
		}
	}
}

func TestMIGMode_PreReset(t *testing.T) {
	t.Run("destroys partitions by PCI bus ID", func(t *testing.T) {
		var calls []string
		exec := &fakeExecutor{
			fn: func(_ context.Context, name string, args ...string) ([]byte, error) {
				calls = append(calls, name+" "+strings.Join(args, " "))
				return []byte("ok\n"), nil
			},
		}
		m := newTestMode(Config{PartitionSize: "1g.5gb"}, &fakeMIGDevices{}, defaultFakeModeNVML(), exec)
		if err := m.PreReset(context.Background(), []PhysicalGPU{"nvidia0", "nvidia2"}); err != nil {
			t.Fatalf("PreReset failed: %v", err)
		}
		want := []string{
			nvidiaSMI + " mig -dci -i 00000000:00:04.0",
			nvidiaSMI + " mig -dgi -i 00000000:00:04.0",
			nvidiaSMI + " mig -dci -i 00000000:85:00.0",
			nvidiaSMI + " mig -dgi -i 00000000:85:00.0",
		}
		if !reflect.DeepEqual(calls, want) {
			t.Fatalf("calls = %v, want %v", calls, want)
		}
	})

	t.Run("in-use translates to Abort CodeGPUBusy with physical GPU ID", func(t *testing.T) {
		exec := &fakeExecutor{
			fn: func(_ context.Context, _ string, args ...string) ([]byte, error) {
				if len(args) >= 2 && args[1] == "-dgi" {
					return []byte("Unable to destroy GPU instance ID 2 on GPU 00000000:00:04.0: In use by another client\n"), errors.New("exit status 1")
				}
				return []byte("ok\n"), nil
			},
		}
		m := newTestMode(Config{PartitionSize: "1g.5gb"}, &fakeMIGDevices{}, defaultFakeModeNVML(), exec)
		err := m.PreReset(context.Background(), []PhysicalGPU{"nvidia0"})
		var abort *Abort
		if !errors.As(err, &abort) {
			t.Fatalf("expected *Abort, got %T (%v)", err, err)
		}
		if abort.Code != CodeGPUBusy || !strings.Contains(abort.Message, "nvidia0/gi2") {
			t.Fatalf("abort = %+v, want CodeGPUBusy mentioning nvidia0/gi2", abort)
		}
	})

	t.Run("generic error propagates", func(t *testing.T) {
		exec := &fakeExecutor{
			fn: func(_ context.Context, _ string, _ ...string) ([]byte, error) {
				return []byte("Unexpected internal error\n"), errors.New("exit status 99")
			},
		}
		m := newTestMode(Config{PartitionSize: "1g.5gb"}, &fakeMIGDevices{}, defaultFakeModeNVML(), exec)
		err := m.PreReset(context.Background(), []PhysicalGPU{"nvidia0"})
		var abort *Abort
		if err == nil || errors.As(err, &abort) {
			t.Fatalf("expected non-Abort error, got %v", err)
		}
	})
}

func TestMIGMode_Converge(t *testing.T) {
	lgipOutput := `+-----------------------------------------------------------------------------+
| GPU instance profiles:                                                      |
| GPU   Name             ID    Instances   Memory     P2P    SM    DEC   ENC  |
|                              Free/Total   GiB              CE    JPEG  OFA  |
|=============================================================================|
|   0  MIG 1g.5gb        19     7/7        4.75       No     14     0     0   |
+-----------------------------------------------------------------------------+
`
	lgiMatchingOutput := `+-----------------------------------------------------------------------------+
| GPU instances:                                                              |
| GPU   Name             Profile  Instance   Placement                        |
|                        ID       ID         Start:Size                       |
|=============================================================================|
|   0  MIG 1g.5gb        19       1          0:1                              |
|   0  MIG 1g.5gb        19       2          1:1                              |
|   0  MIG 1g.5gb        19       3          2:1                              |
|   0  MIG 1g.5gb        19       4          3:1                              |
|   0  MIG 1g.5gb        19       5          4:1                              |
|   0  MIG 1g.5gb        19       6          5:1                              |
|   0  MIG 1g.5gb        19       7          6:1                              |
+-----------------------------------------------------------------------------+
`
	lgiEmptyOutput := `+-----------------------------------------------------------------------------+
| GPU instances:                                                              |
| GPU   Name             Profile  Instance   Placement                        |
|                        ID       ID         Start:Size                       |
|=============================================================================|
| No MIG-devices found                                                        |
+-----------------------------------------------------------------------------+
`
	t.Run("skips already partitioned GPU and recreates unpartitioned GPU", func(t *testing.T) {
		var calls []string
		exec := &fakeExecutor{
			fn: func(_ context.Context, name string, args ...string) ([]byte, error) {
				cmd := strings.Join(args, " ")
				calls = append(calls, name+" "+cmd)
				switch cmd {
				case "mig -lgip -i 00000000:00:04.0", "mig -lgip -i 00000000:00:05.0":
					return []byte(lgipOutput), nil
				case "mig -lgi -i 00000000:00:04.0":
					return []byte(lgiMatchingOutput), nil
				case "mig -lgi -i 00000000:00:05.0":
					return []byte(lgiEmptyOutput), nil
				default:
					return []byte("ok\n"), nil
				}
			},
		}
		fakeDevs := &fakeMIGDevices{}
		m := newTestMode(Config{PartitionSize: "1g.5gb"}, fakeDevs, defaultFakeModeNVML(), exec)
		if err := m.Converge(context.Background(), []PhysicalGPU{"nvidia0", "nvidia1"}); err != nil {
			t.Fatalf("Converge failed: %v", err)
		}
		for _, c := range calls {
			if strings.Contains(c, "00000000:00:04.0") && (strings.Contains(c, "-dci") || strings.Contains(c, "-dgi") || strings.Contains(c, "-cgi")) {
				t.Errorf("unexpected recreate call for already-partitioned nvidia0: %s", c)
			}
		}
		if !reflect.DeepEqual(fakeDevs.discovered, []string{"nvidia0:1g.5gb", "nvidia1:1g.5gb"}) {
			t.Errorf("discovered = %v, want [nvidia0:1g.5gb nvidia1:1g.5gb]", fakeDevs.discovered)
		}
	})

	t.Run("error cases: nil manager and invalid partition size", func(t *testing.T) {
		mNil := newTestMode(Config{PartitionSize: "1g.5gb"}, nil, defaultFakeModeNVML(), nil)
		if err := mNil.Converge(context.Background(), []PhysicalGPU{"nvidia0"}); err == nil {
			t.Fatal("expected error for nil MIGDevices")
		}
		mBad := newTestMode(Config{PartitionSize: "invalid"}, &fakeMIGDevices{}, defaultFakeModeNVML(), nil)
		if err := mBad.Converge(context.Background(), []PhysicalGPU{"nvidia0"}); err == nil {
			t.Fatal("expected error for invalid partition size")
		}
	})
}

func TestResolvePCIBusID_Errors(t *testing.T) {
	if _, err := resolvePCIBusID(defaultFakeModeNVML(), "gpu0"); err == nil {
		t.Error("expected error for invalid GPU name")
	}
	if _, err := resolvePCIBusID(nil, "nvidia0"); err == nil {
		t.Error("expected error for nil NVML")
	}
	nv := &fakeModeNVML{busIDs: map[PhysicalGPU]string{"nvidia0": "   "}}
	if _, err := resolvePCIBusID(nv, "nvidia0"); err == nil {
		t.Error("expected error for whitespace PCI bus ID")
	}
}
