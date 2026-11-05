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
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"google.golang.org/grpc"
	podresources "k8s.io/kubelet/pkg/apis/podresources/v1alpha1"
)

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
	c.mu.Unlock()
	return nil
}

func (c *fakeClock) Sleeps() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.sleeps...)
}

type fakeNVML struct {
	mu            sync.Mutex
	pci           map[PhysicalGPU]string
	procs         map[PhysicalGPU][]int
	procErr       map[PhysicalGPU]error
	fabricReady   map[PhysicalGPU]bool
	fabricErr     map[PhysicalGPU]error
	ampere        map[PhysicalGPU]bool
	ampereErr     map[PhysicalGPU]error
	onProcesses   func(gpu PhysicalGPU, call int) ([]int, error)
	processCalls  map[PhysicalGPU]int
	onFabricReady func(gpu PhysicalGPU, call int) (bool, error)
	fabricCalls   map[PhysicalGPU]int
	persistence   map[PhysicalGPU]bool
	onSetPm       func(gpu PhysicalGPU, enabled bool) error
}

func newFakeNVML() *fakeNVML {
	return &fakeNVML{
		pci:          make(map[PhysicalGPU]string),
		procs:        make(map[PhysicalGPU][]int),
		procErr:      make(map[PhysicalGPU]error),
		fabricReady:  make(map[PhysicalGPU]bool),
		fabricErr:    make(map[PhysicalGPU]error),
		ampere:       make(map[PhysicalGPU]bool),
		ampereErr:    make(map[PhysicalGPU]error),
		processCalls: make(map[PhysicalGPU]int),
		fabricCalls:  make(map[PhysicalGPU]int),
	}
}

func (f *fakeNVML) Init() error     { return nil }
func (f *fakeNVML) Shutdown() error { return nil }

func (f *fakeNVML) PCIBusID(gpu PhysicalGPU) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.pci[gpu]
	if !ok {
		return "", fmt.Errorf("unknown GPU %s", gpu)
	}
	return id, nil
}

func (f *fakeNVML) Processes(gpu PhysicalGPU) ([]int, error) {
	f.mu.Lock()
	f.processCalls[gpu]++
	call := f.processCalls[gpu]
	fn := f.onProcesses
	if err, ok := f.procErr[gpu]; ok && fn == nil {
		f.mu.Unlock()
		return nil, err
	}
	pids := append([]int(nil), f.procs[gpu]...)
	f.mu.Unlock()
	if fn != nil {
		return fn(gpu, call)
	}
	return pids, nil
}

func (f *fakeNVML) FabricReady(gpu PhysicalGPU) (bool, error) {
	f.mu.Lock()
	f.fabricCalls[gpu]++
	call := f.fabricCalls[gpu]
	fn := f.onFabricReady
	if err, ok := f.fabricErr[gpu]; ok && fn == nil {
		f.mu.Unlock()
		return false, err
	}
	ready, ok := f.fabricReady[gpu]
	f.mu.Unlock()
	if fn != nil {
		return fn(gpu, call)
	}
	if !ok {
		return true, nil
	}
	return ready, nil
}

func (f *fakeNVML) IsAmpere(gpu PhysicalGPU) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.ampereErr[gpu]; ok {
		return false, err
	}
	return f.ampere[gpu], nil
}

func (f *fakeNVML) Persistence(gpu PhysicalGPU) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.persistence != nil {
		if on, ok := f.persistence[gpu]; ok {
			return on, nil
		}
	}
	return true, nil
}

func (f *fakeNVML) SetPersistence(gpu PhysicalGPU, enabled bool) error {
	f.mu.Lock()
	fn := f.onSetPm
	if f.persistence == nil {
		f.persistence = make(map[PhysicalGPU]bool)
	}
	f.persistence[gpu] = enabled
	f.mu.Unlock()
	if fn != nil {
		return fn(gpu, enabled)
	}
	return nil
}

func makeBusID32(s string) [32]int8 {
	var out [32]int8
	for i := 0; i < len(s) && i < len(out)-1; i++ {
		out[i] = int8(s[i])
	}
	return out
}

func TestLowLevelHelpers_ParseMinorCStringAndEvalFabric(t *testing.T) {
	if m, err := parseMinor("nvidia0"); err != nil || m != 0 {
		t.Fatalf("parseMinor(nvidia0) = %d, %v", m, err)
	}
	if m, err := parseMinor("nvidia7"); err != nil || m != 7 {
		t.Fatalf("parseMinor(nvidia7) = %d, %v", m, err)
	}
	for _, invalid := range []PhysicalGPU{"", "0", "gpu0", "nvidia", "nvidia01", "nvidia-1"} {
		if _, err := parseMinor(invalid); err == nil {
			t.Errorf("parseMinor(%q) succeeded, want error", invalid)
		}
	}
	bus := makeBusID32("0000:05:00.0")
	if got := parseCString(bus[:]); got != "0000:05:00.0" {
		t.Fatalf("parseCString = %q, want 0000:05:00.0", got)
	}

	tests := []struct {
		name      string
		fabric    nvml.GpuFabricInfo
		ret       nvml.Return
		wantReady bool
		wantErr   bool
	}{
		{name: "not supported ret", ret: nvml.ERROR_NOT_SUPPORTED, wantReady: true},
		{name: "function not found ret", ret: nvml.ERROR_FUNCTION_NOT_FOUND, wantReady: true},
		{name: "state not supported", fabric: nvml.GpuFabricInfo{State: nvml.GPU_FABRIC_STATE_NOT_SUPPORTED}, ret: nvml.SUCCESS, wantReady: true},
		{name: "state not started", fabric: nvml.GpuFabricInfo{State: nvml.GPU_FABRIC_STATE_NOT_STARTED}, ret: nvml.SUCCESS, wantReady: false},
		{name: "state in progress", fabric: nvml.GpuFabricInfo{State: nvml.GPU_FABRIC_STATE_IN_PROGRESS}, ret: nvml.SUCCESS, wantReady: false},
		{name: "state completed success", fabric: nvml.GpuFabricInfo{State: nvml.GPU_FABRIC_STATE_COMPLETED, Status: uint32(nvml.SUCCESS)}, ret: nvml.SUCCESS, wantReady: true},
		{name: "state completed error", fabric: nvml.GpuFabricInfo{State: nvml.GPU_FABRIC_STATE_COMPLETED, Status: uint32(nvml.ERROR_UNKNOWN)}, ret: nvml.SUCCESS, wantErr: true},
		{name: "nvml error", ret: nvml.ERROR_GPU_IS_LOST, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := evalFabric("nvidia0", tc.fabric, tc.ret)
			if (err != nil) != tc.wantErr || (!tc.wantErr && got != tc.wantReady) {
				t.Fatalf("evalFabric() = (%v, %v), want (%v, err=%v)", got, err, tc.wantReady, tc.wantErr)
			}
		})
	}
}

type fakePodResourcesServer struct {
	resp *podresources.ListPodResourcesResponse
}

func (s *fakePodResourcesServer) List(context.Context, *podresources.ListPodResourcesRequest) (*podresources.ListPodResourcesResponse, error) {
	return s.resp, nil
}

func startFakePodResourcesServer(t *testing.T, resp *podresources.ListPodResourcesResponse) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "pr-test-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	sock := filepath.Join(dir, "kubelet.sock")
	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("Listen(%s): %v", sock, err)
	}
	srv := grpc.NewServer()
	podresources.RegisterPodResourcesListerServer(srv, &fakePodResourcesServer{resp: resp})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() {
		srv.Stop()
		_ = lis.Close()
	})
	return sock
}

func writeFakeProc(t *testing.T, procRoot string, pid int, comm string, cgroup string, fdTargets map[string]string) {
	t.Helper()
	pidDir := filepath.Join(procRoot, strconv.Itoa(pid))
	fdDir := filepath.Join(pidDir, "fd")
	if err := os.MkdirAll(fdDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", fdDir, err)
	}
	if comm != "" {
		if err := os.WriteFile(filepath.Join(pidDir, "comm"), []byte(comm+"\n"), 0o644); err != nil {
			t.Fatalf("WriteFile comm: %v", err)
		}
	}
	if cgroup != "" {
		if err := os.WriteFile(filepath.Join(pidDir, "cgroup"), []byte(cgroup+"\n"), 0o644); err != nil {
			t.Fatalf("WriteFile cgroup: %v", err)
		}
	}
	for fdName, target := range fdTargets {
		if err := os.Symlink(target, filepath.Join(fdDir, fdName)); err != nil {
			t.Fatalf("Symlink(%s -> %s): %v", fdName, target, err)
		}
	}
}

func TestAttribute_PodResourcesAndNVMLProcesses(t *testing.T) {
	procRoot := t.TempDir()
	writeFakeProc(t, procRoot, 4100, "python3",
		"0::/kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod11111111_2222_3333_4444_555555555555.slice/cri-containerd-abc.scope",
		map[string]string{"3": "/dev/nvidia0"},
	)
	writeFakeProc(t, procRoot, 4200, "cuda-train",
		"12:memory:/kubepods/burstable/pod99999999-aaaa-bbbb-cccc-dddddddddddd/container123",
		map[string]string{"4": "/dev/nvidia1"},
	)
	writeFakeProc(t, procRoot, 4300, "nv-fabricmanage", "0::/system.slice/nvidia-fabricmanager.service", nil)
	writeFakeProc(t, procRoot, 9999, "nvidia_gpu", "0::/kubepods/self", nil)

	sock := startFakePodResourcesServer(t, &podresources.ListPodResourcesResponse{
		PodResources: []*podresources.PodResources{
			{
				Namespace: "ml",
				Name:      "trainer-0",
				Containers: []*podresources.ContainerResources{{
					Name: "worker",
					Devices: []*podresources.ContainerDevices{{
						ResourceName: "nvidia.com/gpu",
						DeviceIds:    []string{"nvidia0/vgpu0", "nvidia0/vgpu1"},
					}},
				}},
			},
			{
				Namespace: "ml",
				Name:      "trainer-1",
				Containers: []*podresources.ContainerResources{{
					Name: "worker",
					Devices: []*podresources.ContainerDevices{{
						ResourceName: "nvidia.com/gpu",
						DeviceIds:    []string{"nvidia1/vgpu0"},
					}},
				}},
			},
			{
				Namespace: "default",
				Name:      "cpu-only",
				Containers: []*podresources.ContainerResources{{
					Name: "app",
					Devices: []*podresources.ContainerDevices{{
						ResourceName: "example.com/other",
						DeviceIds:    []string{"nvidia0"},
					}},
				}},
			},
		},
	})

	nv := newFakeNVML()
	nv.procs["nvidia0"] = []int{4100, 4300, 9999}
	nv.procs["nvidia1"] = []int{4200}

	r := NewRunner(Config{MaxSharedClientsPerGPU: 2}, NewMode(Config{MaxSharedClientsPerGPU: 2}), nil, nil, nv)
	r.procRoot = procRoot
	r.selfPID = 9999
	r.podResourcesSocket = sock

	got, err := r.attribute(context.Background(), []PhysicalGPU{"nvidia0", "nvidia1"})
	if err != nil {
		t.Fatalf("attribute() unexpected error: %v", err)
	}
	if len(got["nvidia0"]) != 1 || got["nvidia0"][0].PID != 4100 || got["nvidia0"][0].Comm != "python3" {
		t.Fatalf("attribute()[nvidia0] = %+v, want PID 4100 python3", got["nvidia0"])
	}
	if len(got["nvidia1"]) != 1 || got["nvidia1"][0].PID != 4200 || got["nvidia1"][0].Comm != "cuda-train" {
		t.Fatalf("attribute()[nvidia1] = %+v, want PID 4200 cuda-train", got["nvidia1"])
	}
	if !reflect.DeepEqual(r.status.Devices["nvidia0"].AllocatedPods, []string{"ml/trainer-0"}) {
		t.Fatalf("AllocatedPods[nvidia0] = %v, want [ml/trainer-0]", r.status.Devices["nvidia0"].AllocatedPods)
	}
	if !reflect.DeepEqual(r.status.Devices["nvidia1"].AllocatedPods, []string{"ml/trainer-1"}) {
		t.Fatalf("AllocatedPods[nvidia1] = %v, want [ml/trainer-1]", r.status.Devices["nvidia1"].AllocatedPods)
	}
}

func TestWaitForIdle_FastExitAndTier1FDWait(t *testing.T) {
	t.Run("immediate fast exit when clear", func(t *testing.T) {
		clk := newFakeClock()
		r := NewRunner(Config{}, NewMode(Config{}), nil, nil, newFakeNVML())
		r.procRoot = t.TempDir()
		r.now = clk.Now
		r.sleep = clk.Sleep

		if err := r.waitForIdle(context.Background(), []PhysicalGPU{"nvidia0"}, nil); err != nil {
			t.Fatalf("waitForIdle() error: %v", err)
		}
		if len(clk.Sleeps()) != 0 {
			t.Fatalf("waitForIdle() slept %v on idle GPU, want 0 sleeps", clk.Sleeps())
		}
	})

	t.Run("waits for both NVML context release and /proc/pid/fd close then fast-exits", func(t *testing.T) {
		procRoot := t.TempDir()
		writeFakeProc(t, procRoot, 5001, "pytorch", "0::/kubepods/pod-uid-1", map[string]string{"5": "/dev/nvidia0"})

		clk := newFakeClock()
		nv := newFakeNVML()
		nv.onProcesses = func(gpu PhysicalGPU, call int) ([]int, error) {
			switch call {
			case 1:
				return []int{5001}, nil
			case 2:
				return nil, nil
			default:
				_ = os.Remove(filepath.Join(procRoot, "5001", "fd", "5"))
				return nil, nil
			}
		}

		r := NewRunner(Config{}, NewMode(Config{}), nil, nil, nv)
		r.procRoot = procRoot
		r.now = clk.Now
		r.sleep = clk.Sleep

		candidates := map[PhysicalGPU][]BlockingProcess{
			"nvidia0": {{PID: 5001, Comm: "pytorch", Cgroup: "/kubepods/pod-uid-1"}},
		}
		if err := r.waitForIdle(context.Background(), []PhysicalGPU{"nvidia0"}, candidates); err != nil {
			t.Fatalf("waitForIdle() unexpected error: %v", err)
		}
		if want := []time.Duration{time.Second, time.Second}; !reflect.DeepEqual(clk.Sleeps(), want) {
			t.Fatalf("waitForIdle() sleeps = %v, want %v", clk.Sleeps(), want)
		}
	})

	t.Run("times out after 30s and returns Abort CodeGPUBusy with Blocking populated", func(t *testing.T) {
		procRoot := t.TempDir()
		writeFakeProc(t, procRoot, 6001, "stuck-train", "0::/kubepods/pod-stuck", map[string]string{"3": "/dev/nvidia0"})

		clk := newFakeClock()
		nv := newFakeNVML()
		nv.procs["nvidia0"] = []int{6001}

		r := NewRunner(Config{}, NewMode(Config{}), nil, nil, nv)
		r.procRoot = procRoot
		r.now = clk.Now
		r.sleep = clk.Sleep

		err := r.waitForIdle(context.Background(), []PhysicalGPU{"nvidia0"}, nil)
		var abort *Abort
		if !errors.As(err, &abort) || abort.Code != CodeGPUBusy {
			t.Fatalf("waitForIdle() error = %T (%v), want *Abort CodeGPUBusy", err, err)
		}
		if !strings.Contains(abort.Message, "timed out after 30s waiting for GPU processes to exit") {
			t.Fatalf("abort.Message = %q, want prefix 'timed out after 30s waiting for GPU processes to exit'", abort.Message)
		}
		if got := abort.Blocking["nvidia0"]; len(got) != 1 || got[0].PID != 6001 || got[0].Comm != "stuck-train" {
			t.Fatalf("abort.Blocking[nvidia0] = %+v, want PID 6001 stuck-train", got)
		}
		if len(clk.Sleeps()) != 30 {
			t.Fatalf("waitForIdle() sleep count = %d, want 30", len(clk.Sleeps()))
		}
	})
}

func TestGate_AllowlistKernelThreadsPersistencedAndProcRaces(t *testing.T) {
	t.Run("passes with /proc/pid/exe symlinks, 15-char truncated comm fallback, kernel threads, and self", func(t *testing.T) {
		procRoot := t.TempDir()

		// Self PID holding /dev/nvidia0 -> skipped.
		writeFakeProc(t, procRoot, 100, "nvidia_gpu", "0::/kubepods/self", map[string]string{"3": "/dev/nvidia0"})
		// Kernel thread by PF_KTHREAD in /proc/<pid>/stat -> skipped.
		writeFakeProc(t, procRoot, 200, "nvidia-modeset", "", map[string]string{"3": "/dev/nvidia0"})
		_ = os.WriteFile(filepath.Join(procRoot, "200", "stat"), []byte("200 (nvidia-modeset) S 1 0 0 0 -1 2097152\n"), 0o644)
		// Kernel thread by status Kthread: 1 -> skipped.
		writeFakeProc(t, procRoot, 201, "nv_queue", "", map[string]string{"3": "/dev/nvidia0"})
		_ = os.WriteFile(filepath.Join(procRoot, "201", "status"), []byte("Name:\tnv_queue\nKthread:\t1\n"), 0o644)
		// Kernel thread by bracketed comm -> skipped.
		writeFakeProc(t, procRoot, 202, "[kworker/0:1]", "", map[string]string{"3": "/dev/nvidia0"})

		// Allowlisted daemons via real 15-char truncated Linux /proc/<pid>/comm (when /proc/<pid>/exe is unreadable):
		// "nv-fabricmanager" (16 chars) -> "nv-fabricmanage" (15 chars)
		// "nvidia-cuda-mps-control" / "nvidia-cuda-mps-server" -> "nvidia-cuda-mps" (15 chars)
		writeFakeProc(t, procRoot, 300, "nv-fabricmanage", "0::/system.slice/nvidia-fabricmanager.service", map[string]string{"4": "/dev/nvidia0"})
		writeFakeProc(t, procRoot, 301, "nvidia-cuda-mps", "0::/system.slice/nvidia-mps.service", map[string]string{"4": "/dev/nvidia0"})

		// Allowlisted daemons via /proc/<pid>/exe symlink (even with arbitrary or deleted suffix):
		writeFakeProc(t, procRoot, 302, "nv-fabricmanage", "0::/system.slice/nvidia-fabricmanager.service", map[string]string{"4": "/dev/nvidia0"})
		_ = os.Symlink("/usr/bin/nv-fabricmanager", filepath.Join(procRoot, "302", "exe"))
		writeFakeProc(t, procRoot, 303, "nvidia-cuda-mps", "0::/system.slice/nvidia-mps.service", map[string]string{"4": "/dev/nvidia0"})
		_ = os.Symlink("/usr/bin/nvidia-cuda-mps-control (deleted)", filepath.Join(procRoot, "303", "exe"))
		writeFakeProc(t, procRoot, 304, "nvidia-cuda-mps", "0::/system.slice/nvidia-mps.service", map[string]string{"4": "/dev/nvidia0"})
		_ = os.Symlink("/usr/bin/nvidia-cuda-mps-server", filepath.Join(procRoot, "304", "exe"))

		// Foreign process holding non-target GPU /dev/nvidia2 -> skipped.
		writeFakeProc(t, procRoot, 400, "python", "0::/kubepods/other-gpu", map[string]string{"3": "/dev/nvidia2"})
		// Mid-walk race: PID directory without fd subdirectory -> must not error.
		_ = os.MkdirAll(filepath.Join(procRoot, "500"), 0o755)

		r := NewRunner(Config{}, NewMode(Config{}), nil, nil, newFakeNVML())
		r.procRoot = procRoot
		r.selfPID = 100

		if err := r.gate(context.Background(), []PhysicalGPU{"nvidia0"}); err != nil {
			t.Fatalf("gate() unexpected error: %v", err)
		}
	})

	t.Run("rejects process spoofing comm when /proc/pid/exe points to non-allowlisted binary", func(t *testing.T) {
		procRoot := t.TempDir()
		writeFakeProc(t, procRoot, 666, "nv-fabricmanage", "0::/kubepods/pod-spoof", map[string]string{"3": "/dev/nvidia0"})
		_ = os.Symlink("/usr/bin/python3", filepath.Join(procRoot, "666", "exe"))

		clk := newFakeClock()
		r := NewRunner(Config{}, NewMode(Config{}), nil, nil, newFakeNVML())
		r.procRoot = procRoot
		r.selfPID = 100
		r.now = clk.Now
		r.sleep = clk.Sleep

		err := r.gate(context.Background(), []PhysicalGPU{"nvidia0"})
		var abort *Abort
		if !errors.As(err, &abort) || abort.Code != CodeGPUBusy {
			t.Fatalf("gate() err = %v, want *Abort CodeGPUBusy for spoofed comm", err)
		}
		if len(abort.Blocking["nvidia0"]) != 1 || abort.Blocking["nvidia0"][0].PID != 666 {
			t.Fatalf("abort.Blocking[nvidia0] = %+v, want PID 666", abort.Blocking["nvidia0"])
		}
	})

	t.Run("blocks when nvidia-persistenced or foreign monitor holds target GPU", func(t *testing.T) {
		procRoot := t.TempDir()
		writeFakeProc(t, procRoot, 777, "nvidia-persiste", "0::/system.slice/nvidia-persistenced.service", map[string]string{
			"3": "/dev/nvidia0",
			"4": "/dev/nvidia0",
		})
		_ = os.Symlink("/usr/bin/nvidia-persistenced", filepath.Join(procRoot, "777", "exe"))

		clk := newFakeClock()
		r := NewRunner(Config{}, NewMode(Config{}), nil, nil, newFakeNVML())
		r.procRoot = procRoot
		r.selfPID = 100
		r.now = clk.Now
		r.sleep = clk.Sleep

		err := r.gate(context.Background(), []PhysicalGPU{"nvidia0"})
		var abort *Abort
		if !errors.As(err, &abort) || abort.Code != CodeGPUBusy {
			t.Fatalf("gate() err = %v, want *Abort CodeGPUBusy", err)
		}
		if len(abort.Blocking["nvidia0"]) != 1 || abort.Blocking["nvidia0"][0].PID != 777 {
			t.Fatalf("abort.Blocking[nvidia0] = %+v, want single deduplicated PID 777", abort.Blocking["nvidia0"])
		}
	})

	t.Run("holder persisting past shared idle deadline gives Error-GPU-Busy with remaining budget and last-walk blockers only", func(t *testing.T) {
		procRoot := t.TempDir()
		writeFakeProc(t, procRoot, 901, "pytorch-worker", "0::/kubepods/pod-train", map[string]string{"3": "/dev/nvidia0"})
		writeFakeProc(t, procRoot, 902, "dcgm-exporter", "0::/kubepods/pod-dcgm", map[string]string{"4": "/dev/nvidia0"})
		writeFakeProc(t, procRoot, 903, "stuck-agent", "0::/kubepods/pod-stuck", map[string]string{"5": "/dev/nvidia0"})

		clk := newFakeClock()
		nv := newFakeNVML()
		nv.pci["nvidia0"] = "0000:05:00.0"
		nv.onProcesses = func(gpu PhysicalGPU, call int) ([]int, error) {
			if call <= 23 {
				return []int{901}, nil
			}
			_ = os.RemoveAll(filepath.Join(procRoot, "901"))
			return nil, nil
		}

		pmDisabledBeforeGateWalk := false
		gateSleeps := 0
		nv.onSetPm = func(gpu PhysicalGPU, enabled bool) error {
			if gpu == "nvidia0" && !enabled && gateSleeps == 0 {
				pmDisabledBeforeGateWalk = true
			}
			return nil
		}
		fe := &fakeExecutor{}

		devs := newFakeDevices([]PhysicalGPU{"nvidia0"}, nil, nil)
		r := NewRunner(Config{}, NewMode(Config{}), devs, nil, nv)
		r.procRoot = procRoot
		r.devDir = t.TempDir()
		r.selfPID = 100
		r.run = fe
		r.now = clk.Now
		r.sleep = func(ctx context.Context, d time.Duration) error {
			if r.status.LastRequest.Phase == PhaseGate {
				gateSleeps++
				if gateSleeps == 2 {
					_ = os.RemoveAll(filepath.Join(procRoot, "902"))
				}
			}
			return clk.Sleep(ctx, d)
		}

		st := r.Run(context.Background(), "0")
		if st.LastRequest.OverallStatus != CodeGPUBusy || st.LastRequest.Phase != PhaseGate {
			t.Fatalf("status = (%q, %q), want (%q, %q)", st.LastRequest.OverallStatus, st.LastRequest.Phase, CodeGPUBusy, PhaseGate)
		}
		if !pmDisabledBeforeGateWalk {
			t.Fatal("expected NVML SetPersistence(false) to run before the first gate walk")
		}
		if len(fe.Calls()) != 0 {
			t.Fatalf("expected 0 exec calls, got %+v", fe.Calls())
		}
		if gateSleeps != 8 || len(clk.Sleeps()) != 30 {
			t.Fatalf("gateSleeps=%d totalSleeps=%d, want 8 and 30", gateSleeps, len(clk.Sleeps()))
		}
		wantBlockers := []BlockingProcess{{PID: 903, Comm: "stuck-agent", Cgroup: "/kubepods/pod-stuck"}}
		if !reflect.DeepEqual(st.Devices["nvidia0"].BlockingPIDs, wantBlockers) {
			t.Fatalf("BlockingPIDs = %+v, want %+v", st.Devices["nvidia0"].BlockingPIDs, wantBlockers)
		}
	})
}

func TestResetGPUs_SuccessRetriesInUseAndDeadline(t *testing.T) {
	t.Run("succeeds on third attempt with backoff and jitter issuing exactly one exec call per attempt", func(t *testing.T) {
		clk := newFakeClock()
		resetCalls := 0
		fe := &fakeExecutor{
			fn: func(ctx context.Context, bin string, args ...string) ([]byte, error) {
				resetCalls++
				if resetCalls < 3 {
					return []byte("Unable to reset GPU"), errors.New("exit status 1")
				}
				return []byte("GPU 0000:05:00.0 was successfully reset.\nAll done.\n"), nil
			},
		}

		r := NewRunner(Config{}, NewMode(Config{}), nil, nil, newFakeNVML())
		r.run = fe
		r.now = clk.Now
		r.sleep = clk.Sleep
		r.jitter = func(base time.Duration) time.Duration { return 250 * time.Millisecond }

		res := r.resetGPUs(context.Background(), map[PhysicalGPU]string{"nvidia0": "0000:05:00.0"})
		got := res["nvidia0"]
		if got.Err != nil || got.InUse || got.Attempts != 3 {
			t.Fatalf("resetGPUs[nvidia0] = %+v, want Attempts=3, InUse=false, Err=nil", got)
		}
		wantSleeps := []time.Duration{2*time.Second + 250*time.Millisecond, 4*time.Second + 250*time.Millisecond}
		if !reflect.DeepEqual(clk.Sleeps(), wantSleeps) {
			t.Fatalf("sleeps = %v, want %v", clk.Sleeps(), wantSleeps)
		}
		wantArgs := [][]string{
			{"--gpu-reset", "-i", "0000:05:00.0"},
			{"--gpu-reset", "-i", "0000:05:00.0"},
			{"--gpu-reset", "-i", "0000:05:00.0"},
		}
		calls := fe.Calls()
		if len(calls) != len(wantArgs) {
			t.Fatalf("got %d exec calls, want %d (exactly 1 per attempt)", len(calls), len(wantArgs))
		}
		for i, want := range wantArgs {
			if calls[i].Bin != nvidiaSMI || !reflect.DeepEqual(calls[i].Args, want) {
				t.Fatalf("call[%d] = %s %v, want %s %v", i, calls[i].Bin, calls[i].Args, nvidiaSMI, want)
			}
		}
	})

	t.Run("exact hardware output table (#118): 8-GPU success, 1 and 4 busy, unknown BDF exit 0, exit 6, and retry only failed subset", func(t *testing.T) {
		eightGPUs := map[PhysicalGPU]string{
			"nvidia0": "00000000:8F:00.0",
			"nvidia1": "00000000:90:00.0",
			"nvidia2": "00000000:96:00.0",
			"nvidia3": "00000000:97:00.0",
			"nvidia4": "00000000:C4:00.0",
			"nvidia5": "00000000:C5:00.0",
			"nvidia6": "00000000:CB:00.0",
			"nvidia7": "00000000:CC:00.0",
		}
		const want8BDFList = "00000000:8F:00.0,00000000:90:00.0,00000000:96:00.0,00000000:97:00.0,00000000:C4:00.0,00000000:C5:00.0,00000000:CB:00.0,00000000:CC:00.0"

		t.Run("all 8 GPUs succeed in a single list call (~43s within 120s deadline)", func(t *testing.T) {
			clk := newFakeClock()
			fe := &fakeExecutor{
				fn: func(ctx context.Context, bin string, args ...string) ([]byte, error) {
					clk.Advance(43 * time.Second)
					return []byte(
						"GPU 00000000:8F:00.0 was successfully reset.\n" +
							"GPU 00000000:90:00.0 was successfully reset.\n" +
							"GPU 00000000:96:00.0 was successfully reset.\n" +
							"GPU 00000000:97:00.0 was successfully reset.\n" +
							"GPU 00000000:C4:00.0 was successfully reset.\n" +
							"GPU 00000000:C5:00.0 was successfully reset.\n" +
							"GPU 00000000:CB:00.0 was successfully reset.\n" +
							"GPU 00000000:CC:00.0 was successfully reset.\n" +
							"All done.\n",
					), nil
				},
			}
			r := NewRunner(Config{}, NewMode(Config{}), nil, nil, newFakeNVML())
			r.run = fe
			r.now = clk.Now
			r.sleep = clk.Sleep

			res := r.resetGPUs(context.Background(), eightGPUs)
			calls := fe.Calls()
			if len(calls) != 1 || !reflect.DeepEqual(calls[0].Args, []string{"--gpu-reset", "-i", want8BDFList}) {
				t.Fatalf("calls = %+v, want single list call with %s", calls, want8BDFList)
			}
			for gpu := range eightGPUs {
				if got := res[gpu]; got.Err != nil || got.InUse || got.Attempts != 1 {
					t.Fatalf("res[%s] = %+v, want Attempts=1, InUse=false, Err=nil", gpu, got)
				}
			}
		})

		t.Run("1 busy GPU out of 8 (exit 255): marks busy GPU InUse and does not retry", func(t *testing.T) {
			clk := newFakeClock()
			const out1Busy = `The following GPUs could not be reset:
  GPU 00000000:97:00.0: In use by another client

1 device is currently being used by one or more other processes (e.g., Fabric Manager, CUDA application, graphics application such as an X server, or a monitoring application such as another instance of nvidia-smi). Please first kill all processes using this device and all compute applications running in the system.
`
			fe := &fakeExecutor{
				fn: func(ctx context.Context, bin string, args ...string) ([]byte, error) {
					return []byte(out1Busy), errors.New("exit status 255")
				},
			}
			r := NewRunner(Config{}, NewMode(Config{}), nil, nil, newFakeNVML())
			r.run = fe
			r.now = clk.Now
			r.sleep = clk.Sleep

			res := r.resetGPUs(context.Background(), eightGPUs)
			if len(fe.Calls()) != 1 {
				t.Fatalf("got %d exec calls, want 1 (no retry on InUse)", len(fe.Calls()))
			}
			for gpu := range eightGPUs {
				got := res[gpu]
				wantInUse := gpu == "nvidia3"
				if got.Err == nil || got.Attempts != 1 || got.InUse != wantInUse {
					t.Fatalf("res[%s] = %+v, want Attempts=1, InUse=%v, Err!=nil", gpu, got, wantInUse)
				}
			}
		})

		t.Run("4 busy GPUs out of 8 (exit 255): marks all 4 busy GPUs InUse and does not retry", func(t *testing.T) {
			clk := newFakeClock()
			const out4Busy = `The following GPUs could not be reset:
  GPU 00000000:8F:00.0: In use by another client
  GPU 00000000:90:00.0: In use by another client
  GPU 00000000:96:00.0: In use by another client
  GPU 00000000:97:00.0: In use by another client

4 devices are currently being used by one or more other processes (e.g., Fabric Manager, CUDA application, graphics application such as an X server, or a monitoring application such as another instance of nvidia-smi). Please first kill all processes using these devices and all compute applications running in the system.
`
			fe := &fakeExecutor{
				fn: func(ctx context.Context, bin string, args ...string) ([]byte, error) {
					return []byte(out4Busy), errors.New("exit status 255")
				},
			}
			r := NewRunner(Config{}, NewMode(Config{}), nil, nil, newFakeNVML())
			r.run = fe
			r.now = clk.Now
			r.sleep = clk.Sleep

			res := r.resetGPUs(context.Background(), eightGPUs)
			if len(fe.Calls()) != 1 {
				t.Fatalf("got %d exec calls, want 1 (no retry on InUse)", len(fe.Calls()))
			}
			busySet := map[PhysicalGPU]bool{"nvidia0": true, "nvidia1": true, "nvidia2": true, "nvidia3": true}
			for gpu := range eightGPUs {
				got := res[gpu]
				if got.Err == nil || got.Attempts != 1 || got.InUse != busySet[gpu] {
					t.Fatalf("res[%s] = %+v, want Attempts=1, InUse=%v, Err!=nil", gpu, got, busySet[gpu])
				}
			}
		})

		t.Run("unknown BDF silently ignored by nvidia-smi with exit 0 is treated as failed and retried alone", func(t *testing.T) {
			clk := newFakeClock()
			resetAttempt := 0
			fe := &fakeExecutor{
				fn: func(ctx context.Context, bin string, args ...string) ([]byte, error) {
					if len(args) >= 3 && args[0] == "--gpu-reset" {
						resetAttempt++
						if resetAttempt == 1 {
							// Exit 0, but only 8F was reset; FF was silently skipped!
							return []byte("GPU 00000000:8F:00.0 was successfully reset.\nAll done.\n"), nil
						}
						return []byte("No devices were found\n"), errors.New("exit status 6")
					}
					return nil, nil
				},
			}
			r := NewRunner(Config{}, NewMode(Config{}), nil, nil, newFakeNVML())
			r.run = fe
			r.now = clk.Now
			r.sleep = clk.Sleep
			r.jitter = func(time.Duration) time.Duration { return 0 }

			res := r.resetGPUs(context.Background(), map[PhysicalGPU]string{
				"nvidia0": "00000000:8F:00.0",
				"nvidia1": "00000000:FF:00.0",
			})
			if res["nvidia0"].Err != nil || res["nvidia0"].Attempts != 1 {
				t.Fatalf("res[nvidia0] = %+v, want Attempts=1, Err=nil", res["nvidia0"])
			}
			if res["nvidia1"].Err == nil || res["nvidia1"].InUse || res["nvidia1"].Attempts != 5 || !strings.Contains(res["nvidia1"].Err.Error(), "No devices were found") {
				t.Fatalf("res[nvidia1] = %+v, want Attempts=5, InUse=false, Err containing 'No devices were found'", res["nvidia1"])
			}
			// Verify total exec calls is 5 (exactly 1 per attempt, no persistence exec toggles): attempt 1 passed both BDFs, and retries passed ONLY 00000000:FF:00.0.
			wantCalls := []execCall{
				{Bin: nvidiaSMI, Args: []string{"--gpu-reset", "-i", "00000000:8F:00.0,00000000:FF:00.0"}},
				{Bin: nvidiaSMI, Args: []string{"--gpu-reset", "-i", "00000000:FF:00.0"}},
				{Bin: nvidiaSMI, Args: []string{"--gpu-reset", "-i", "00000000:FF:00.0"}},
				{Bin: nvidiaSMI, Args: []string{"--gpu-reset", "-i", "00000000:FF:00.0"}},
				{Bin: nvidiaSMI, Args: []string{"--gpu-reset", "-i", "00000000:FF:00.0"}},
			}
			if gotCalls := fe.Calls(); !reflect.DeepEqual(gotCalls, wantCalls) {
				t.Fatalf("exec calls = %+v, want %+v", gotCalls, wantCalls)
			}
		})

		t.Run("transient partial failure retries only failed GPUs in a single list call", func(t *testing.T) {
			clk := newFakeClock()
			attempt := 0
			fe := &fakeExecutor{
				fn: func(ctx context.Context, bin string, args ...string) ([]byte, error) {
					attempt++
					if attempt == 1 {
						return []byte("GPU 00000000:8F:00.0 was successfully reset.\n"), errors.New("exit status 1")
					}
					return []byte("GPU 00000000:90:00.0 was successfully reset.\nGPU 00000000:96:00.0 was successfully reset.\nAll done.\n"), nil
				},
			}
			r := NewRunner(Config{}, NewMode(Config{}), nil, nil, newFakeNVML())
			r.run = fe
			r.now = clk.Now
			r.sleep = clk.Sleep
			r.jitter = func(time.Duration) time.Duration { return 0 }

			res := r.resetGPUs(context.Background(), map[PhysicalGPU]string{
				"nvidia0": "00000000:8f:00.0", // lowercase to verify case-insensitive BDF matching
				"nvidia1": "00000000:90:00.0",
				"nvidia2": "00000000:96:00.0",
			})
			if res["nvidia0"].Err != nil || res["nvidia0"].Attempts != 1 {
				t.Fatalf("res[nvidia0] = %+v, want Attempts=1, Err=nil", res["nvidia0"])
			}
			for _, gpu := range []PhysicalGPU{"nvidia1", "nvidia2"} {
				if res[gpu].Err != nil || res[gpu].Attempts != 2 {
					t.Fatalf("res[%s] = %+v, want Attempts=2, Err=nil", gpu, res[gpu])
				}
			}
			wantCalls := []execCall{
				{Bin: nvidiaSMI, Args: []string{"--gpu-reset", "-i", "00000000:8f:00.0,00000000:90:00.0,00000000:96:00.0"}},
				{Bin: nvidiaSMI, Args: []string{"--gpu-reset", "-i", "00000000:90:00.0,00000000:96:00.0"}},
			}
			if gotCalls := fe.Calls(); !reflect.DeepEqual(gotCalls, wantCalls) {
				t.Fatalf("exec calls = %+v, want %+v", gotCalls, wantCalls)
			}
		})
	})

	t.Run("exhausts all 5 attempts on persistent hardware error", func(t *testing.T) {
		clk := newFakeClock()
		fe := &fakeExecutor{
			fn: func(ctx context.Context, bin string, args ...string) ([]byte, error) {
				if len(args) >= 1 && args[0] == "--gpu-reset" {
					return []byte("GPU fallen off the bus"), errors.New("exit status 1")
				}
				return nil, nil
			},
		}

		r := NewRunner(Config{}, NewMode(Config{}), nil, nil, newFakeNVML())
		r.run = fe
		r.now = clk.Now
		r.sleep = clk.Sleep
		r.jitter = func(time.Duration) time.Duration { return 100 * time.Millisecond }

		res := r.resetGPUs(context.Background(), map[PhysicalGPU]string{"nvidia0": "0000:05:00.0"})
		got := res["nvidia0"]
		if got.Err == nil || got.InUse || got.Attempts != 5 {
			t.Fatalf("resetGPUs[nvidia0] = %+v, want Attempts=5, InUse=false, Err!=nil", got)
		}
	})

	t.Run("in use by another process sets InUse and does not retry", func(t *testing.T) {
		clk := newFakeClock()
		fe := &fakeExecutor{
			fn: func(ctx context.Context, bin string, args ...string) ([]byte, error) {
				return []byte("GPU is currently in use by another process."), errors.New("exit status 1")
			},
		}

		r := NewRunner(Config{}, NewMode(Config{}), nil, nil, newFakeNVML())
		r.run = fe
		r.now = clk.Now
		r.sleep = clk.Sleep

		res := r.resetGPUs(context.Background(), map[PhysicalGPU]string{
			"nvidia0": "0000:05:00.0",
			"nvidia1": "0000:06:00.0",
		})
		for _, gpu := range []PhysicalGPU{"nvidia0", "nvidia1"} {
			if !res[gpu].InUse || res[gpu].Attempts != 1 || res[gpu].Err == nil {
				t.Fatalf("resetGPUs[%s] = %+v, want InUse=true, Attempts=1, Err!=nil", gpu, res[gpu])
			}
		}
		if len(clk.Sleeps()) != 0 {
			t.Fatalf("sleeps = %v, want 0 sleeps on in-use refusal", clk.Sleeps())
		}
	})

	t.Run("enforces 120s deadline per GPU", func(t *testing.T) {
		clk := newFakeClock()
		fe := &fakeExecutor{
			fn: func(ctx context.Context, bin string, args ...string) ([]byte, error) {
				if len(args) >= 1 && args[0] == "--gpu-reset" {
					clk.Advance(55 * time.Second)
					return []byte("timeout"), errors.New("exit status 1")
				}
				return nil, nil
			},
		}

		r := NewRunner(Config{}, NewMode(Config{}), nil, nil, newFakeNVML())
		r.run = fe
		r.now = clk.Now
		r.sleep = clk.Sleep
		r.jitter = func(time.Duration) time.Duration { return 100 * time.Millisecond }

		res := r.resetGPUs(context.Background(), map[PhysicalGPU]string{"nvidia0": "0000:05:00.0"})
		// Attempt 1 (55s) + backoff 1 (2.1s -> 57.1s) + Attempt 2 (55s -> 112.1s) + backoff 2 (4.1s -> 116.2s) + Attempt 3 (55s -> >120s) stops at 3 attempts.
		if res["nvidia0"].Attempts != 3 || res["nvidia0"].Err == nil {
			t.Fatalf("resetGPUs[nvidia0] = %+v, want 3 attempts capped by 120s deadline", res["nvidia0"])
		}
	})

	t.Run("realJitter returns positive bounded jitter", func(t *testing.T) {
		for _, base := range resetBackoffs {
			if j := realJitter(base); j <= 0 || j > base/4 {
				t.Fatalf("realJitter(%v) = %v, want in (0, %v]", base, j, base/4)
			}
		}
	})
}

func TestWaitFabric_NonFabricReadyAndTimeout(t *testing.T) {
	t.Run("returns immediately on non-fabric GPUs", func(t *testing.T) {
		clk := newFakeClock()
		nv := newFakeNVML()
		nv.fabricReady["nvidia0"] = true
		nv.fabricReady["nvidia1"] = true

		r := NewRunner(Config{}, NewMode(Config{}), nil, nil, nv)
		r.now = clk.Now
		r.sleep = clk.Sleep

		if err := r.waitFabric(context.Background(), []PhysicalGPU{"nvidia0", "nvidia1"}); err != nil {
			t.Fatalf("waitFabric() error: %v", err)
		}
		if len(clk.Sleeps()) != 0 {
			t.Fatalf("sleeps = %v, want 0", clk.Sleeps())
		}
	})

	t.Run("polls until fabric training completes", func(t *testing.T) {
		clk := newFakeClock()
		nv := newFakeNVML()
		nv.onFabricReady = func(gpu PhysicalGPU, call int) (bool, error) { return call >= 4, nil }

		r := NewRunner(Config{}, NewMode(Config{}), nil, nil, nv)
		r.now = clk.Now
		r.sleep = clk.Sleep

		if err := r.waitFabric(context.Background(), []PhysicalGPU{"nvidia0"}); err != nil {
			t.Fatalf("waitFabric() error: %v", err)
		}
		if len(clk.Sleeps()) != 3 {
			t.Fatalf("sleeps = %d, want 3", len(clk.Sleeps()))
		}
	})

	t.Run("times out after 60s if fabric never completes", func(t *testing.T) {
		clk := newFakeClock()
		nv := newFakeNVML()
		nv.fabricReady["nvidia0"] = false

		r := NewRunner(Config{}, NewMode(Config{}), nil, nil, nv)
		r.now = clk.Now
		r.sleep = clk.Sleep

		err := r.waitFabric(context.Background(), []PhysicalGPU{"nvidia0"})
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("waitFabric() err = %v, want timeout error", err)
		}
		if len(clk.Sleeps()) != 60 {
			t.Fatalf("sleeps = %d, want 60", len(clk.Sleeps()))
		}
	})
}

func TestNVMLErr_NotSupportedAndNoPermission(t *testing.T) {
	if err := nvmlErr("GetPersistenceMode(nvidia0)", nvml.SUCCESS); err != nil {
		t.Fatalf("nvmlErr(SUCCESS) = %v, want nil", err)
	}
	if err := nvmlErr("GetPersistenceMode(nvidia0)", nvml.ERROR_NOT_SUPPORTED); err == nil || !strings.Contains(err.Error(), "ERROR_NOT_SUPPORTED") {
		t.Fatalf("nvmlErr(ERROR_NOT_SUPPORTED) = %v, want explicit ERROR_NOT_SUPPORTED error", err)
	}
	if err := nvmlErr("SetPersistenceMode(nvidia0,false)", nvml.ERROR_NO_PERMISSION); err == nil || !strings.Contains(err.Error(), "ERROR_NO_PERMISSION") {
		t.Fatalf("nvmlErr(ERROR_NO_PERMISSION) = %v, want explicit ERROR_NO_PERMISSION error", err)
	}
}
