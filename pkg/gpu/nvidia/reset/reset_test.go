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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia/gpusharing"
	"github.com/prometheus/client_golang/prometheus"
	podresources "k8s.io/kubelet/pkg/apis/podresources/v1alpha1"
)

type execCall struct {
	Bin  string
	Args []string
}

type fakeExecutor struct {
	mu    sync.Mutex
	calls []execCall
	fn    func(ctx context.Context, bin string, args ...string) ([]byte, error)
	log   *[]string
}

func (e *fakeExecutor) Run(ctx context.Context, bin string, args ...string) ([]byte, error) {
	e.mu.Lock()
	e.calls = append(e.calls, execCall{Bin: bin, Args: append([]string(nil), args...)})
	if e.log != nil {
		*e.log = append(*e.log, fmt.Sprintf("exec(%s %s)", filepath.Base(bin), strings.Join(args, " ")))
	}
	fn := e.fn
	e.mu.Unlock()
	var out []byte
	var err error
	if fn != nil {
		out, err = fn(ctx, bin, args...)
	}
	if len(out) == 0 && err == nil && len(args) >= 3 && args[0] == "--gpu-reset" && args[1] == "-i" {
		var b strings.Builder
		for _, bdf := range strings.Split(args[2], ",") {
			fmt.Fprintf(&b, "GPU %s was successfully reset.\n", bdf)
		}
		b.WriteString("All done.\n")
		return []byte(b.String()), nil
	}
	return out, err
}

func (e *fakeExecutor) Calls() []execCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]execCall(nil), e.calls...)
}

type stepClock struct {
	mu   sync.Mutex
	cur  time.Time
	step time.Duration
}

func newStepClock(start time.Time, step time.Duration) *stepClock {
	return &stepClock{cur: start, step: step}
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.cur
	c.cur = c.cur.Add(c.step)
	return t
}

type fakeDevices struct {
	mu          sync.Mutex
	physical    []PhysicalGPU
	health      map[string]Health
	shareBase   bool // when true, maps "nvidia0/vgpuN" to base "nvidia0" like manager.go
	setHealthFn func(id string, h Health) error
	log         *[]string
}

func newFakeDevices(phys []PhysicalGPU, initial map[string]Health, log *[]string) *fakeDevices {
	h := make(map[string]Health, len(initial))
	for k, v := range initial {
		h[k] = v
	}
	for _, g := range phys {
		if _, ok := h[string(g)]; !ok {
			h[string(g)] = Healthy
		}
	}
	return &fakeDevices{physical: phys, health: h, log: log}
}

func (d *fakeDevices) key(id string) string {
	if d.shareBase {
		if base, _, ok := strings.Cut(id, "/vgpu"); ok {
			return base
		}
	}
	return id
}

func (d *fakeDevices) Physical() []PhysicalGPU {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]PhysicalGPU(nil), d.physical...)
}

func (d *fakeDevices) Health(id string) Health {
	d.mu.Lock()
	defer d.mu.Unlock()
	if h, ok := d.health[d.key(id)]; ok {
		return h
	}
	return Healthy
}

func (d *fakeDevices) SetHealth(id string, h Health) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.health[d.key(id)] = h
	if d.log != nil {
		*d.log = append(*d.log, fmt.Sprintf("SetHealth(%s,%s)", id, h))
	}
	if d.setHealthFn != nil {
		return d.setHealthFn(id, h)
	}
	return nil
}

type fakeMonitors struct {
	mu          sync.Mutex
	running     bool
	startErr    error
	panicOnStop bool
	log         *[]string
}

func (m *fakeMonitors) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.running = false
	if m.log != nil {
		*m.log = append(*m.log, "monitors.Stop")
	}
	if m.panicOnStop {
		panic("boom in monitors.Stop")
	}
}

func (m *fakeMonitors) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.log != nil {
		*m.log = append(*m.log, "monitors.Start")
	}
	if m.startErr != nil {
		return m.startErr
	}
	m.running = true
	return nil
}

func (m *fakeMonitors) isRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

type flowNVML struct {
	mu          sync.Mutex
	initialized bool
	initErr     error
	shutdownErr error
	pci         map[PhysicalGPU]string
	pciErr      map[PhysicalGPU]error
	procs       map[PhysicalGPU][]int
	fabricErr   error
	ampere      bool
	ampereErr   error
	persistence map[PhysicalGPU]bool
	getPmErr    map[PhysicalGPU]error
	setPmErr    map[PhysicalGPU]error
	setPmFn     func(gpu PhysicalGPU, enabled bool) error
	log         *[]string
}

func newFlowNVML(pci map[PhysicalGPU]string, log *[]string) *flowNVML {
	return &flowNVML{initialized: true, pci: pci, log: log}
}

func (n *flowNVML) Init() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.log != nil {
		*n.log = append(*n.log, "nvml.Init")
	}
	if n.initErr != nil {
		return n.initErr
	}
	n.initialized = true
	return nil
}

func (n *flowNVML) Shutdown() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.log != nil {
		*n.log = append(*n.log, "nvml.Shutdown")
	}
	if n.shutdownErr != nil {
		return n.shutdownErr
	}
	n.initialized = false
	return nil
}

func (n *flowNVML) PCIBusID(gpu PhysicalGPU) (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.log != nil {
		*n.log = append(*n.log, fmt.Sprintf("nvml.PCIBusID(%s)", gpu))
	}
	if err, ok := n.pciErr[gpu]; ok && err != nil {
		return "", err
	}
	bus, ok := n.pci[gpu]
	if !ok {
		return "", fmt.Errorf("unknown GPU %s", gpu)
	}
	return bus, nil
}

func (n *flowNVML) Processes(gpu PhysicalGPU) ([]int, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]int(nil), n.procs[gpu]...), nil
}

func (n *flowNVML) FabricReady(gpu PhysicalGPU) (bool, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.fabricErr != nil {
		return false, n.fabricErr
	}
	return true, nil
}

func (n *flowNVML) IsAmpere(gpu PhysicalGPU) (bool, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.ampere, n.ampereErr
}

func (n *flowNVML) Persistence(gpu PhysicalGPU) (bool, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.initialized {
		return false, errors.New("nvml not initialized")
	}
	if err, ok := n.getPmErr[gpu]; ok && err != nil {
		return false, err
	}
	if n.persistence != nil {
		if on, ok := n.persistence[gpu]; ok {
			return on, nil
		}
	}
	return true, nil
}

func (n *flowNVML) SetPersistence(gpu PhysicalGPU, enabled bool) error {
	n.mu.Lock()
	if !n.initialized {
		n.mu.Unlock()
		return errors.New("nvml not initialized")
	}
	if n.log != nil {
		*n.log = append(*n.log, fmt.Sprintf("nvml.SetPersistence(%s,%t)", gpu, enabled))
	}
	fn := n.setPmFn
	err := n.setPmErr[gpu]
	n.mu.Unlock()
	if fn != nil {
		if fErr := fn(gpu, enabled); fErr != nil {
			return fErr
		}
	}
	if err != nil {
		return err
	}
	n.mu.Lock()
	if n.persistence == nil {
		n.persistence = make(map[PhysicalGPU]bool)
	}
	n.persistence[gpu] = enabled
	n.mu.Unlock()
	return nil
}

func (n *flowNVML) persistenceState(gpu PhysicalGPU) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if on, ok := n.persistence[gpu]; ok {
		return on
	}
	return true
}

func (n *flowNVML) isInitialized() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.initialized
}

type fakeMode struct {
	mu          sync.Mutex
	ids         map[PhysicalGPU][]string
	preResetFn  func(ctx context.Context, gpus []PhysicalGPU) error
	convergeFn  func(ctx context.Context, gpus []PhysicalGPU) error
	convergedOn [][]PhysicalGPU
	log         *[]string
}

func (m *fakeMode) DeviceIDs(gpu PhysicalGPU) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ids, ok := m.ids[gpu]; ok {
		return ids
	}
	return []string{string(gpu)}
}

func (m *fakeMode) PreReset(ctx context.Context, gpus []PhysicalGPU) error {
	m.mu.Lock()
	if m.log != nil {
		*m.log = append(*m.log, fmt.Sprintf("mode.PreReset(%v)", gpus))
	}
	fn := m.preResetFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, gpus)
	}
	return nil
}

func (m *fakeMode) Converge(ctx context.Context, gpus []PhysicalGPU) error {
	m.mu.Lock()
	m.convergedOn = append(m.convergedOn, append([]PhysicalGPU(nil), gpus...))
	if m.log != nil {
		*m.log = append(*m.log, fmt.Sprintf("mode.Converge(%v)", gpus))
	}
	fn := m.convergeFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, gpus)
	}
	return nil
}

func newTestFlowRunner(t *testing.T, cfg Config, mode Mode, devs Devices, mon Monitors, nv NVML, exec *fakeExecutor) *Runner {
	t.Helper()
	r := NewRunner(cfg, mode, devs, mon, nv)
	r.procRoot = t.TempDir()
	r.devDir = t.TempDir()
	r.podResourcesSocket = filepath.Join(t.TempDir(), "missing.sock")
	if exec != nil {
		r.run = exec
	} else {
		r.run = &fakeExecutor{}
	}
	r.jitter = func(time.Duration) time.Duration { return 0 }
	r.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	return r
}

func TestParseTarget(t *testing.T) {
	fourGPUs := []PhysicalGPU{"nvidia2", "nvidia0", "nvidia3", "nvidia1"}
	tests := []struct {
		name     string
		raw      string
		physical []PhysicalGPU
		want     Target
		wantErr  bool
	}{
		{name: "all sorts and deduplicates", raw: "all", physical: []PhysicalGPU{"nvidia2", "nvidia0", "nvidia1", "nvidia0"}, want: Target{Raw: "all", All: true, GPUs: []PhysicalGPU{"nvidia0", "nvidia1", "nvidia2"}}},
		{name: "single GPU index 0", raw: "0", physical: fourGPUs, want: Target{Raw: "0", All: false, GPUs: []PhysicalGPU{"nvidia0"}}},
		{name: "multiple hyphen-separated sorted", raw: "3-0-2", physical: fourGPUs, want: Target{Raw: "3-0-2", All: false, GPUs: []PhysicalGPU{"nvidia0", "nvidia2", "nvidia3"}}},
		{name: "multi-digit sorted numerically", raw: "10-2", physical: []PhysicalGPU{"nvidia2", "nvidia10"}, want: Target{Raw: "10-2", All: false, GPUs: []PhysicalGPU{"nvidia2", "nvidia10"}}},
		{name: "empty raw", raw: "", physical: fourGPUs, wantErr: true},
		{name: "empty physical", raw: "all", physical: nil, wantErr: true},
		{name: "bare hyphen", raw: "-", physical: fourGPUs, wantErr: true},
		{name: "leading hyphen", raw: "-0", physical: fourGPUs, wantErr: true},
		{name: "trailing hyphen", raw: "0-", physical: fourGPUs, wantErr: true},
		{name: "double hyphen", raw: "0--1", physical: fourGPUs, wantErr: true},
		{name: "leading zero 00", raw: "00", physical: fourGPUs, wantErr: true},
		{name: "leading zero 01", raw: "01", physical: fourGPUs, wantErr: true},
		{name: "leading zero second segment", raw: "0-01", physical: fourGPUs, wantErr: true},
		{name: "duplicate index", raw: "0-0", physical: fourGPUs, wantErr: true},
		{name: "non-adjacent duplicate", raw: "1-2-1", physical: fourGPUs, wantErr: true},
		{name: "out-of-range index", raw: "4", physical: fourGPUs, wantErr: true},
		{name: "mixed valid and out-of-range", raw: "0-7", physical: fourGPUs, wantErr: true},
		{name: "force suffix", raw: "0-force", physical: fourGPUs, wantErr: true},
		{name: "all-force suffix", raw: "all-force", physical: fourGPUs, wantErr: true},
		{name: "comma separator", raw: "0,1", physical: fourGPUs, wantErr: true},
		{name: "whitespace", raw: " 0 ", physical: fourGPUs, wantErr: true},
		{name: "plus sign", raw: "+0", physical: fourGPUs, wantErr: true},
		{name: "overflow", raw: "999999999999999999999999999999999", physical: fourGPUs, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseTarget(tc.raw, tc.physical)
			if tc.wantErr {
				var ab *Abort
				if !errors.As(err, &ab) || ab.Code != CodeInvalidTarget {
					t.Fatalf("ParseTarget(%q) err = %v, want *Abort{CodeInvalidTarget}", tc.raw, err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ParseTarget(%q) = (%+v, %v), want (%+v, nil)", tc.raw, got, err, tc.want)
			}
		})
	}
}

func TestCheckPlatform(t *testing.T) {
	emptyDev := t.TempDir()
	nvswitchDev := t.TempDir()
	if err := os.WriteFile(filepath.Join(nvswitchDev, "nvidia-nvswitch0"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	subsetTarget := Target{Raw: "0", All: false, GPUs: []PhysicalGPU{"nvidia0"}}
	allTarget := Target{Raw: "all", All: true, GPUs: []PhysicalGPU{"nvidia0", "nvidia1"}}

	tests := []struct {
		name      string
		cfg       Config
		devDir    string
		ampere    bool
		ampereErr error
		target    Target
		wantCode  Code
	}{
		{name: "plain node subset allowed", cfg: Config{FractionDivisor: 1}, devDir: emptyDev, target: subsetTarget},
		{name: "fractional vGPU refused", cfg: Config{FractionDivisor: 2}, devDir: emptyDev, target: allTarget, wantCode: CodeUnsupportedNode},
		{name: "A2 node (Ampere + nvswitch) refused", cfg: Config{FractionDivisor: 1}, devDir: nvswitchDev, ampere: true, target: allTarget, wantCode: CodeUnsupportedNode},
		{name: "Hopper NVSwitch allowed", cfg: Config{FractionDivisor: 1}, devDir: nvswitchDev, ampere: false, target: subsetTarget},
		{name: "Ampere without NVSwitch allowed", cfg: Config{FractionDivisor: 1}, devDir: emptyDev, ampere: true, target: subsetTarget},
		{name: "NVSwitch with NVML arch error returns CodeNVMLFailed", cfg: Config{FractionDivisor: 1}, devDir: nvswitchDev, ampereErr: errors.New("nvml down"), target: subsetTarget, wantCode: CodeNVMLFailed},
		{name: "MPS with subset refused", cfg: Config{Sharing: gpusharing.MPS, MaxSharedClientsPerGPU: 4}, devDir: emptyDev, target: subsetTarget, wantCode: CodeUnsupportedMode},
		{name: "MPS with all refused", cfg: Config{Sharing: gpusharing.MPS, MaxSharedClientsPerGPU: 4}, devDir: emptyDev, target: allTarget, wantCode: CodeUnsupportedMode},
		{name: "time-sharing with subset allowed", cfg: Config{Sharing: gpusharing.TimeSharing, MaxSharedClientsPerGPU: 4}, devDir: emptyDev, target: subsetTarget},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			nv := &flowNVML{ampere: tc.ampere, ampereErr: tc.ampereErr}
			r := NewRunner(tc.cfg, NewMode(tc.cfg, WithNVML(nv)), nil, nil, nv)
			r.devDir = tc.devDir
			err := r.checkPlatform(tc.target)
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("checkPlatform unexpected error: %v", err)
				}
				return
			}
			var ab *Abort
			if !errors.As(err, &ab) || ab.Code != tc.wantCode {
				t.Fatalf("checkPlatform err = %v, want *Abort{%s}", err, tc.wantCode)
			}
		})
	}
}

func TestRunnerRun_HappyPath_DerivesHealthNotPriorHealth(t *testing.T) {
	var log []string
	devs := newFakeDevices(
		[]PhysicalGPU{"nvidia0", "nvidia1"},
		map[string]Health{
			"nvidia0/gi0": Unhealthy,
			"nvidia0/gi1": Unhealthy,
			"nvidia1/gi0": Healthy,
			"nvidia1/gi1": Healthy,
		},
		&log,
	)
	mon := &fakeMonitors{running: true, log: &log}
	nv := newFlowNVML(map[PhysicalGPU]string{"nvidia0": "0000:05:00.0", "nvidia1": "0000:06:00.0"}, &log)
	mode := &fakeMode{
		ids: map[PhysicalGPU][]string{
			"nvidia0": {"nvidia0/gi0", "nvidia0/gi1"},
			"nvidia1": {"nvidia1/gi0", "nvidia1/gi1"},
		},
		log: &log,
	}
	exec := &fakeExecutor{}
	clk := newStepClock(time.Date(2026, 11, 3, 1, 0, 0, 0, time.UTC), time.Second)

	r := newTestFlowRunner(t, Config{PartitionSize: "1g.5gb"}, mode, devs, mon, nv, exec)
	r.now = clk.Now
	r.podResourcesSocket = startFakePodResourcesServer(t, &podresources.ListPodResourcesResponse{
		PodResources: []*podresources.PodResources{{
			Namespace: "default",
			Name:      "train-0",
			Containers: []*podresources.ContainerResources{{
				Name: "main",
				Devices: []*podresources.ContainerDevices{{
					ResourceName: "nvidia.com/gpu",
					DeviceIds:    []string{"nvidia0/gi0"},
				}},
			}},
		}},
	})

	status := r.Run(context.Background(), "0-1")
	if status.LastRequest.OverallStatus != CodeSucceeded || status.LastRequest.Phase != PhaseDone {
		t.Fatalf("status = (%q, %q), want (%q, %q)", status.LastRequest.OverallStatus, status.LastRequest.Phase, CodeSucceeded, PhaseDone)
	}
	for _, id := range []string{"nvidia0/gi0", "nvidia0/gi1", "nvidia1/gi0", "nvidia1/gi1"} {
		if got := devs.Health(id); got != Healthy {
			t.Errorf("device %s health = %q, want %q", id, got, Healthy)
		}
	}
	if !nv.isInitialized() || !mon.isRunning() {
		t.Errorf("nvml=%v monitors=%v, want both running", nv.isInitialized(), mon.isRunning())
	}
	ds0 := status.Devices["nvidia0"]
	if ds0.PCIBusID != "0000:05:00.0" || ds0.Status != CodeSucceeded || ds0.Attempts != 1 || !reflect.DeepEqual(ds0.AllocatedPods, []string{"default/train-0"}) {
		t.Errorf("nvidia0 status = %+v", ds0)
	}
	rawJSON, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("json.Marshal(status): %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(rawJSON, &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if lastReq, _ := decoded["lastRequest"].(map[string]any); lastReq["overallStatus"] != string(CodeSucceeded) {
		t.Errorf("unexpected lastRequest JSON: %v", lastReq)
	}
}

func TestRunnerRun_PreResetAbortsAndLIFOUndo(t *testing.T) {
	t.Run("MPSNode_RejectedInValidateBeforeMarkUnhealthyOrWaitForIdle", func(t *testing.T) {
		var log []string
		devs := newFakeDevices([]PhysicalGPU{"nvidia0", "nvidia1"}, map[string]Health{"nvidia0": Healthy, "nvidia1": Healthy}, &log)
		mon := &fakeMonitors{running: true, log: &log}
		nv := newFlowNVML(map[PhysicalGPU]string{"nvidia0": "0000:05:00.0", "nvidia1": "0000:06:00.0"}, &log)
		cfg := Config{Sharing: gpusharing.MPS, MaxSharedClientsPerGPU: 4}
		r := newTestFlowRunner(t, cfg, NewMode(cfg, WithNVML(nv)), devs, mon, nv, nil)

		for _, target := range []string{"0", "all"} {
			st := r.Run(context.Background(), target)
			if st.LastRequest.OverallStatus != CodeUnsupportedMode || st.LastRequest.Phase != PhaseValidate {
				t.Fatalf("Run(%q) = (%q, %q), want (%q, %q)", target, st.LastRequest.OverallStatus, st.LastRequest.Phase, CodeUnsupportedMode, PhaseValidate)
			}
			if st.LastRequest.Message != "GPU reset is not supported on MPS nodes" {
				t.Fatalf("Run(%q) Message = %q, want %q", target, st.LastRequest.Message, "GPU reset is not supported on MPS nodes")
			}
			if len(log) != 0 {
				t.Fatalf("Run(%q) performed unexpected operations before rejection: %v", target, log)
			}
		}
	})

	t.Run("WaitForIdleTimeout_RestoresPriorHealthAndPopulatesBlockingPIDs", func(t *testing.T) {
		var log []string
		devs := newFakeDevices([]PhysicalGPU{"nvidia0", "nvidia1"}, map[string]Health{"nvidia0": Unhealthy, "nvidia1": Healthy}, &log)
		mon := &fakeMonitors{running: true, log: &log}
		nv := newFlowNVML(map[PhysicalGPU]string{"nvidia0": "0000:05:00.0", "nvidia1": "0000:06:00.0"}, &log)
		nv.procs = map[PhysicalGPU][]int{"nvidia0": {1234}}

		clk := newFakeClock()
		r := newTestFlowRunner(t, Config{}, NewMode(Config{}, WithNVML(nv)), devs, mon, nv, nil)
		r.now = clk.Now
		r.sleep = clk.Sleep
		writeFakeProc(t, r.procRoot, 1234, "python3", "0::/kubepods/pod123", map[string]string{"3": "/dev/nvidia0"})

		status := r.Run(context.Background(), "0-1")
		if status.LastRequest.OverallStatus != CodeGPUBusy || status.LastRequest.Phase != PhaseWaitForIdle {
			t.Fatalf("status = (%q, %q), want (%q, %q)", status.LastRequest.OverallStatus, status.LastRequest.Phase, CodeGPUBusy, PhaseWaitForIdle)
		}
		if !strings.Contains(status.LastRequest.Message, "timed out after 30s waiting for GPU processes to exit") {
			t.Errorf("Message = %q, want prefix 'timed out after 30s waiting for GPU processes to exit'", status.LastRequest.Message)
		}
		if devs.Health("nvidia0") != Unhealthy || devs.Health("nvidia1") != Healthy {
			t.Errorf("priorHealth not restored: nvidia0=%q nvidia1=%q", devs.Health("nvidia0"), devs.Health("nvidia1"))
		}
		if len(status.Devices["nvidia0"].BlockingPIDs) != 1 || status.Devices["nvidia0"].BlockingPIDs[0].PID != 1234 {
			t.Errorf("nvidia0 BlockingPIDs = %+v, want PID 1234", status.Devices["nvidia0"].BlockingPIDs)
		}
	})

	t.Run("VGPUSharedPhysicalEntry_AbortRestoresHealthy", func(t *testing.T) {
		// Regression test for Bug 3 (#90/#91): 2 vgpu IDs ("nvidia0/vgpu0", "nvidia0/vgpu1")
		// share one physical GPU health entry ("nvidia0") in manager.go.
		// Snapshotting priorHealth for all IDs before setting any to Unhealthy ensures an abort leaves "nvidia0" Healthy.
		devs := newFakeDevices([]PhysicalGPU{"nvidia0"}, map[string]Health{"nvidia0": Healthy}, nil)
		devs.shareBase = true
		mon := &fakeMonitors{running: true}
		nv := newFlowNVML(map[PhysicalGPU]string{"nvidia0": "0000:05:00.0"}, nil)
		nv.pciErr = map[PhysicalGPU]error{"nvidia0": errors.New("nvml pci lookup failed after marking unhealthy")}

		cfg := Config{Sharing: gpusharing.TimeSharing, MaxSharedClientsPerGPU: 2}
		r := newTestFlowRunner(t, cfg, NewMode(cfg, WithNVML(nv)), devs, mon, nv, nil)

		status := r.Run(context.Background(), "0")
		if status.LastRequest.OverallStatus != CodeNVMLFailed || status.LastRequest.Phase != PhaseResolve {
			t.Fatalf("status = (%q, %q), want (%q, %q)", status.LastRequest.OverallStatus, status.LastRequest.Phase, CodeNVMLFailed, PhaseResolve)
		}
		for _, id := range []string{"nvidia0", "nvidia0/vgpu0", "nvidia0/vgpu1"} {
			if got := devs.Health(id); got != Healthy {
				t.Errorf("Health(%q) after abort = %q, want %q", id, got, Healthy)
			}
		}
	})

	t.Run("GateFailure_UnwindsPersistenceThenPriorHealth", func(t *testing.T) {
		var log []string
		devs := newFakeDevices([]PhysicalGPU{"nvidia0"}, map[string]Health{"nvidia0": Healthy}, &log)
		mon := &fakeMonitors{running: true, log: &log}
		nv := newFlowNVML(map[PhysicalGPU]string{"nvidia0": "0000:05:00.0"}, &log)
		exec := &fakeExecutor{log: &log}

		clk := newFakeClock()
		r := newTestFlowRunner(t, Config{}, NewMode(Config{}, WithNVML(nv)), devs, mon, nv, exec)
		r.now = clk.Now
		r.sleep = clk.Sleep
		writeFakeProc(t, r.procRoot, 4321, "dcgm-exporter", "0::/kubepods/dcgm", map[string]string{"3": "/dev/nvidia0"})

		status := r.Run(context.Background(), "0")
		if status.LastRequest.OverallStatus != CodeGPUBusy || status.LastRequest.Phase != PhaseGate {
			t.Fatalf("status = (%q, %q), want (%q, %q)", status.LastRequest.OverallStatus, status.LastRequest.Phase, CodeGPUBusy, PhaseGate)
		}
		wantLog := []string{
			"SetHealth(nvidia0,Unhealthy)",
			"nvml.PCIBusID(nvidia0)",
			"nvml.SetPersistence(nvidia0,false)",
			"nvml.SetPersistence(nvidia0,true)",
			"SetHealth(nvidia0,Healthy)",
		}
		if !reflect.DeepEqual(log, wantLog) {
			t.Errorf("LIFO undo mismatch:\ngot:  %v\nwant: %v", log, wantLog)
		}
		if calls := exec.Calls(); len(calls) != 0 {
			t.Errorf("expected 0 exec calls (persistence via NVML), got %+v", calls)
		}
	})

	t.Run("PreResetFailure_UnwindsConvergeThenPersistenceThenPriorHealth", func(t *testing.T) {
		var log []string
		devs := newFakeDevices([]PhysicalGPU{"nvidia0"}, map[string]Health{"nvidia0": Healthy}, &log)
		mon := &fakeMonitors{running: true, log: &log}
		nv := newFlowNVML(map[PhysicalGPU]string{"nvidia0": "0000:05:00.0"}, &log)
		mode := &fakeMode{
			preResetFn: func(ctx context.Context, gpus []PhysicalGPU) error {
				return &Abort{Code: CodeGPUBusy, Message: "MIG instance gi0 in use"}
			},
			log: &log,
		}
		exec := &fakeExecutor{log: &log}
		r := newTestFlowRunner(t, Config{}, mode, devs, mon, nv, exec)

		status := r.Run(context.Background(), "0")
		if status.LastRequest.OverallStatus != CodeGPUBusy || status.LastRequest.Phase != PhasePreReset {
			t.Fatalf("status = (%q, %q), want (%q, %q)", status.LastRequest.OverallStatus, status.LastRequest.Phase, CodeGPUBusy, PhasePreReset)
		}
		wantLog := []string{
			"SetHealth(nvidia0,Unhealthy)",
			"nvml.PCIBusID(nvidia0)",
			"nvml.SetPersistence(nvidia0,false)",
			"mode.PreReset([nvidia0])",
			"mode.Converge([nvidia0])",
			"nvml.SetPersistence(nvidia0,true)",
			"SetHealth(nvidia0,Healthy)",
		}
		if !reflect.DeepEqual(log, wantLog) {
			t.Errorf("LIFO undo mismatch:\ngot:  %v\nwant: %v", log, wantLog)
		}
	})

	t.Run("ResetDriverInUse_UnwindsFullStackInLIFOOrder", func(t *testing.T) {
		var log []string
		devs := newFakeDevices([]PhysicalGPU{"nvidia0"}, map[string]Health{"nvidia0": Healthy}, &log)
		mon := &fakeMonitors{running: true, log: &log}
		nv := newFlowNVML(map[PhysicalGPU]string{"nvidia0": "0000:05:00.0"}, &log)
		mode := &fakeMode{log: &log}

		var r *Runner
		exec := &fakeExecutor{
			log: &log,
			fn: func(ctx context.Context, bin string, args ...string) ([]byte, error) {
				if len(args) >= 1 && args[0] == "--gpu-reset" {
					// Late client opens /dev/nvidia0 right as --gpu-reset runs.
					writeFakeProc(t, r.procRoot, 8888, "late-cuda", "0::/kubepods/late", map[string]string{"3": "/dev/nvidia0"})
					return []byte("GPU 0000:05:00.0 is currently in use by another process"), errors.New("exit status 1")
				}
				return nil, nil
			},
		}
		r = newTestFlowRunner(t, Config{}, mode, devs, mon, nv, exec)

		status := r.Run(context.Background(), "0")
		if status.LastRequest.OverallStatus != CodeGPUBusy || status.LastRequest.Phase != PhaseReset {
			t.Fatalf("status = (%q, %q), want (%q, %q)", status.LastRequest.OverallStatus, status.LastRequest.Phase, CodeGPUBusy, PhaseReset)
		}
		if !nv.isInitialized() || !mon.isRunning() || devs.Health("nvidia0") != Healthy {
			t.Errorf("nvml=%v mon=%v health=%q", nv.isInitialized(), mon.isRunning(), devs.Health("nvidia0"))
		}
		if len(status.Devices["nvidia0"].BlockingPIDs) != 1 || status.Devices["nvidia0"].BlockingPIDs[0].PID != 8888 {
			t.Errorf("nvidia0 BlockingPIDs = %+v, want late PID 8888", status.Devices["nvidia0"].BlockingPIDs)
		}
		wantLog := []string{
			"SetHealth(nvidia0,Unhealthy)",
			"nvml.PCIBusID(nvidia0)",
			"nvml.SetPersistence(nvidia0,false)",
			"mode.PreReset([nvidia0])",
			"monitors.Stop",
			"nvml.Shutdown",
			"exec(nvidia-smi --gpu-reset -i 0000:05:00.0)",
			"nvml.Init",
			"monitors.Start",
			"mode.Converge([nvidia0])",
			"nvml.SetPersistence(nvidia0,true)",
			"SetHealth(nvidia0,Healthy)",
		}
		if !reflect.DeepEqual(log, wantLog) {
			t.Errorf("InUse rollback sequence mismatch:\ngot:  %v\nwant: %v", log, wantLog)
		}
	})

	t.Run("MixedPersistenceSnapshot_DisablesAndRestoresOnlyEnabledGPUs_NoPmExecs", func(t *testing.T) {
		// Happy path: nvidia0=on, nvidia1=off, nvidia2=on -> only nvidia0 and nvidia2 are disabled and re-enabled; nvidia1 stays off.
		var log []string
		gpus := []PhysicalGPU{"nvidia0", "nvidia1", "nvidia2"}
		devs := newFakeDevices(gpus, nil, nil)
		mon := &fakeMonitors{running: true}
		nv := newFlowNVML(map[PhysicalGPU]string{
			"nvidia0": "0000:05:00.0",
			"nvidia1": "0000:06:00.0",
			"nvidia2": "0000:07:00.0",
		}, &log)
		nv.persistence = map[PhysicalGPU]bool{"nvidia0": true, "nvidia1": false, "nvidia2": true}
		exec := &fakeExecutor{
			fn: func(ctx context.Context, bin string, args ...string) ([]byte, error) {
				// Simulate --gpu-reset leaving persistence disabled on all reset GPUs.
				nv.mu.Lock()
				for _, g := range gpus {
					nv.persistence[g] = false
				}
				nv.mu.Unlock()
				return nil, nil
			},
		}
		r := newTestFlowRunner(t, Config{}, &fakeMode{}, devs, mon, nv, exec)

		st := r.Run(context.Background(), "all")
		if st.LastRequest.OverallStatus != CodeSucceeded {
			t.Fatalf("OverallStatus = %q (%s), want %q", st.LastRequest.OverallStatus, st.LastRequest.Message, CodeSucceeded)
		}
		if !nv.persistenceState("nvidia0") || nv.persistenceState("nvidia1") || !nv.persistenceState("nvidia2") {
			t.Fatalf("final persistence = (%t, %t, %t), want (true, false, true)", nv.persistenceState("nvidia0"), nv.persistenceState("nvidia1"), nv.persistenceState("nvidia2"))
		}
		for _, entry := range log {
			if strings.Contains(entry, "SetPersistence(nvidia1") {
				t.Fatalf("unexpected SetPersistence call on off GPU nvidia1 in log: %v", log)
			}
		}
		calls := exec.Calls()
		if len(calls) != 1 || calls[0].Args[0] != "--gpu-reset" {
			t.Fatalf("exec calls = %+v, want exactly 1 (--gpu-reset only, zero persistence execs)", calls)
		}

		// Abort path with mixed {on, off}: nvidia0=true, nvidia1=false; Gate fails -> only nvidia0 restored to true, nvidia1 untouched.
		log = nil
		nvAbort := newFlowNVML(map[PhysicalGPU]string{"nvidia0": "0000:05:00.0", "nvidia1": "0000:06:00.0"}, &log)
		nvAbort.persistence = map[PhysicalGPU]bool{"nvidia0": true, "nvidia1": false}
		execAbort := &fakeExecutor{}
		rAbort := newTestFlowRunner(t, Config{}, &fakeMode{}, newFakeDevices([]PhysicalGPU{"nvidia0", "nvidia1"}, nil, nil), mon, nvAbort, execAbort)
		writeFakeProc(t, rAbort.procRoot, 4321, "dcgm-exporter", "0::/kubepods/dcgm", map[string]string{"3": "/dev/nvidia1"})

		stAbort := rAbort.Run(context.Background(), "0-1")
		if stAbort.LastRequest.OverallStatus != CodeGPUBusy || stAbort.LastRequest.Phase != PhaseGate {
			t.Fatalf("abort status = (%q, %q), want (%q, %q)", stAbort.LastRequest.OverallStatus, stAbort.LastRequest.Phase, CodeGPUBusy, PhaseGate)
		}
		if !nvAbort.persistenceState("nvidia0") || nvAbort.persistenceState("nvidia1") {
			t.Fatalf("abort persistence = (%t, %t), want (true, false)", nvAbort.persistenceState("nvidia0"), nvAbort.persistenceState("nvidia1"))
		}
		wantAbortLog := []string{
			"nvml.PCIBusID(nvidia0)",
			"nvml.PCIBusID(nvidia1)",
			"nvml.SetPersistence(nvidia0,false)",
			"nvml.SetPersistence(nvidia0,true)",
		}
		if !reflect.DeepEqual(log, wantAbortLog) {
			t.Fatalf("abort NVML log = %v, want %v", log, wantAbortLog)
		}
		if len(execAbort.Calls()) != 0 {
			t.Fatalf("abort exec calls = %+v, want 0", execAbort.Calls())
		}
	})

	t.Run("NVMLPersistenceErrors_FailLoudlyAndRestoreSnapshot", func(t *testing.T) {
		// 1. SetPersistence(false) returning NO_PERMISSION on nvidia1 fails ReleasePersistence and restores nvidia0.
		nv := newFlowNVML(map[PhysicalGPU]string{"nvidia0": "0000:05:00.0", "nvidia1": "0000:06:00.0"}, nil)
		nv.setPmErr = map[PhysicalGPU]error{"nvidia1": errors.New("nvml SetPersistenceMode: Insufficient Permissions")}
		r := newTestFlowRunner(t, Config{}, &fakeMode{}, newFakeDevices([]PhysicalGPU{"nvidia0", "nvidia1"}, nil, nil), &fakeMonitors{running: true}, nv, nil)
		st := r.Run(context.Background(), "0-1")
		if st.LastRequest.OverallStatus != CodeHardwareFailure || st.LastRequest.Phase != PhaseReleasePersistence {
			t.Fatalf("SetPersistence(false) error = (%q, %q), want (%q, %q)", st.LastRequest.OverallStatus, st.LastRequest.Phase, CodeHardwareFailure, PhaseReleasePersistence)
		}
		if !nv.persistenceState("nvidia0") {
			t.Fatal("expected nvidia0 persistence to be unwound back to true after nvidia1 SetPersistence failure")
		}

		// 2. GetPersistenceMode returning NOT_SUPPORTED fails ReleasePersistence with CodeNVMLFailed.
		nvGet := newFlowNVML(map[PhysicalGPU]string{"nvidia0": "0000:05:00.0"}, nil)
		nvGet.getPmErr = map[PhysicalGPU]error{"nvidia0": errors.New("nvml GetPersistenceMode: Not Supported")}
		rGet := newTestFlowRunner(t, Config{}, &fakeMode{}, newFakeDevices([]PhysicalGPU{"nvidia0"}, nil, nil), &fakeMonitors{running: true}, nvGet, nil)
		stGet := rGet.Run(context.Background(), "0")
		if stGet.LastRequest.OverallStatus != CodeNVMLFailed || stGet.LastRequest.Phase != PhaseReleasePersistence {
			t.Fatalf("Persistence() error = (%q, %q), want (%q, %q)", stGet.LastRequest.OverallStatus, stGet.LastRequest.Phase, CodeNVMLFailed, PhaseReleasePersistence)
		}

		// 3. SetPersistence(true) failure in Converge fails only that GPU (PartialFailure).
		devsConv := newFakeDevices([]PhysicalGPU{"nvidia0", "nvidia1"}, nil, nil)
		nvConv := newFlowNVML(map[PhysicalGPU]string{"nvidia0": "0000:05:00.0", "nvidia1": "0000:06:00.0"}, nil)
		nvConv.setPmFn = func(gpu PhysicalGPU, enabled bool) error {
			if gpu == "nvidia1" && enabled {
				return errors.New("nvml SetPersistenceMode: Insufficient Permissions")
			}
			return nil
		}
		modeConv := &fakeMode{}
		rConv := newTestFlowRunner(t, Config{}, modeConv, devsConv, &fakeMonitors{running: true}, nvConv, nil)
		stConv := rConv.Run(context.Background(), "0-1")
		if stConv.LastRequest.OverallStatus != CodePartialFailure || stConv.Devices["nvidia1"].Status != CodeReconcileFailed {
			t.Fatalf("Converge pm error = (%q, %q), want (PartialFailure, Error-Reconcile-Failed)", stConv.LastRequest.OverallStatus, stConv.Devices["nvidia1"].Status)
		}
		if devsConv.Health("nvidia0") != Healthy || devsConv.Health("nvidia1") != Unhealthy {
			t.Fatalf("health = (%q, %q), want (Healthy, Unhealthy)", devsConv.Health("nvidia0"), devsConv.Health("nvidia1"))
		}
		if len(modeConv.convergedOn) != 1 || !reflect.DeepEqual(modeConv.convergedOn[0], []PhysicalGPU{"nvidia0"}) {
			t.Fatalf("convergedOn = %v, want [[nvidia0]]", modeConv.convergedOn)
		}
	})
}

func TestRunnerRun_PostResetOutcomes(t *testing.T) {
	t.Run("AllHardwareFailures_NeverRestoresPriorHealthy", func(t *testing.T) {
		devs := newFakeDevices([]PhysicalGPU{"nvidia0"}, map[string]Health{"nvidia0": Healthy}, nil)
		mon := &fakeMonitors{running: true}
		nv := newFlowNVML(map[PhysicalGPU]string{"nvidia0": "0000:05:00.0"}, nil)
		mode := &fakeMode{}
		exec := &fakeExecutor{
			fn: func(ctx context.Context, bin string, args ...string) ([]byte, error) {
				if len(args) >= 1 && args[0] == "--gpu-reset" {
					return []byte("GPU fallen off the bus"), errors.New("exit status 1")
				}
				return nil, nil
			},
		}
		clk := newFakeClock()
		r := newTestFlowRunner(t, Config{}, mode, devs, mon, nv, exec)
		r.now = clk.Now
		r.sleep = clk.Sleep

		status := r.Run(context.Background(), "0")
		if status.LastRequest.OverallStatus != CodeHardwareFailure || devs.Health("nvidia0") != Unhealthy {
			t.Fatalf("status=%q health=%q, want (%q, Unhealthy)", status.LastRequest.OverallStatus, devs.Health("nvidia0"), CodeHardwareFailure)
		}
		if !nv.isInitialized() || !mon.isRunning() || len(mode.convergedOn) != 0 || status.Devices["nvidia0"].Attempts != 5 {
			t.Errorf("nvml=%v mon=%v convergedOn=%v attempts=%d", nv.isInitialized(), mon.isRunning(), mode.convergedOn, status.Devices["nvidia0"].Attempts)
		}
	})

	t.Run("PartialFailure_ConvergesOnlyRecoveredGPUs", func(t *testing.T) {
		devs := newFakeDevices([]PhysicalGPU{"nvidia0", "nvidia1"}, map[string]Health{"nvidia0": Healthy, "nvidia1": Healthy}, nil)
		mon := &fakeMonitors{running: true}
		nv := newFlowNVML(map[PhysicalGPU]string{"nvidia0": "0000:05:00.0", "nvidia1": "0000:06:00.0"}, nil)
		mode := &fakeMode{}
		exec := &fakeExecutor{
			fn: func(ctx context.Context, bin string, args ...string) ([]byte, error) {
				if len(args) >= 3 && args[0] == "--gpu-reset" {
					if strings.Contains(args[2], "0000:05:00.0") {
						return []byte("GPU 0000:05:00.0 was successfully reset.\nGPU 0000:06:00.0: reset failed on GPU 1\n"), errors.New("exit status 1")
					}
					return []byte("GPU 0000:06:00.0: reset failed on GPU 1\n"), errors.New("exit status 1")
				}
				return nil, nil
			},
		}
		clk := newFakeClock()
		r := newTestFlowRunner(t, Config{}, mode, devs, mon, nv, exec)
		r.now = clk.Now
		r.sleep = clk.Sleep

		status := r.Run(context.Background(), "0-1")
		if status.LastRequest.OverallStatus != CodePartialFailure {
			t.Fatalf("OverallStatus = %q, want %q", status.LastRequest.OverallStatus, CodePartialFailure)
		}
		if devs.Health("nvidia0") != Healthy || devs.Health("nvidia1") != Unhealthy {
			t.Errorf("health nvidia0=%q nvidia1=%q", devs.Health("nvidia0"), devs.Health("nvidia1"))
		}
		if len(mode.convergedOn) != 1 || !reflect.DeepEqual(mode.convergedOn[0], []PhysicalGPU{"nvidia0"}) {
			t.Errorf("mode.Converge called with %v, want [[nvidia0]]", mode.convergedOn)
		}
	})

	t.Run("ConvergeAndFabricFailures_LeaveGPUsUnhealthyWithCodeReconcileFailed", func(t *testing.T) {
		for _, tc := range []struct {
			name      string
			convErr   error
			fabricErr error
		}{
			{name: "converge failure", convErr: errors.New("MIG partition creation failed")},
			{name: "fabric failure", fabricErr: errors.New("fabric training failed")},
		} {
			t.Run(tc.name, func(t *testing.T) {
				devs := newFakeDevices([]PhysicalGPU{"nvidia0"}, map[string]Health{"nvidia0": Healthy}, nil)
				mon := &fakeMonitors{running: true}
				nv := newFlowNVML(map[PhysicalGPU]string{"nvidia0": "0000:05:00.0"}, nil)
				nv.fabricErr = tc.fabricErr
				mode := &fakeMode{convergeFn: func(context.Context, []PhysicalGPU) error { return tc.convErr }}
				r := newTestFlowRunner(t, Config{}, mode, devs, mon, nv, nil)

				status := r.Run(context.Background(), "0")
				if status.LastRequest.OverallStatus != CodeReconcileFailed || devs.Health("nvidia0") != Unhealthy {
					t.Fatalf("status=%q health=%q, want (%q, Unhealthy)", status.LastRequest.OverallStatus, devs.Health("nvidia0"), CodeReconcileFailed)
				}
				if !nv.isInitialized() || !mon.isRunning() {
					t.Errorf("nvml=%v monitors=%v", nv.isInitialized(), mon.isRunning())
				}
			})
		}
	})

	t.Run("PanicAfterReleaseGPUs_StillRestoresNVMLAndMonitors", func(t *testing.T) {
		devs := newFakeDevices([]PhysicalGPU{"nvidia0"}, map[string]Health{"nvidia0": Healthy}, nil)
		mon := &fakeMonitors{running: true}
		nv := newFlowNVML(map[PhysicalGPU]string{"nvidia0": "0000:05:00.0"}, nil)
		mode := &fakeMode{convergeFn: func(context.Context, []PhysicalGPU) error { panic("unexpected panic in Converge") }}
		r := newTestFlowRunner(t, Config{}, mode, devs, mon, nv, nil)

		defer func() {
			if recover() == nil {
				t.Fatal("expected panic to propagate to Watcher defer")
			}
			if !nv.isInitialized() || !mon.isRunning() {
				t.Errorf("nvml=%v monitors=%v after panic, want both restored", nv.isInitialized(), mon.isRunning())
			}
		}()
		_ = r.Run(context.Background(), "0")
	})

	t.Run("MarkUnhealthySendFails_AbortsAndUnwindsAlreadyMarkedDevices", func(t *testing.T) {
		var log []string
		devs := newFakeDevices([]PhysicalGPU{"nvidia0", "nvidia1"}, map[string]Health{"nvidia0": Healthy, "nvidia1": Healthy}, &log)
		devs.setHealthFn = func(id string, h Health) error {
			if id == "nvidia1" && h == Unhealthy {
				return errors.New("timed out sending health update on ListAndWatch")
			}
			return nil
		}
		nv := newFlowNVML(map[PhysicalGPU]string{"nvidia0": "0000:05:00.0", "nvidia1": "0000:06:00.0"}, &log)
		r := newTestFlowRunner(t, Config{}, &fakeMode{log: &log}, devs, &fakeMonitors{running: true, log: &log}, nv, nil)

		status := r.Run(context.Background(), "0-1")
		if status.LastRequest.OverallStatus != CodeReconcileFailed || status.LastRequest.Phase != PhaseMarkUnhealthy {
			t.Fatalf("status = (%q, %q), want (%q, %q)", status.LastRequest.OverallStatus, status.LastRequest.Phase, CodeReconcileFailed, PhaseMarkUnhealthy)
		}
		if devs.Health("nvidia0") != Healthy || devs.Health("nvidia1") != Healthy {
			t.Errorf("health nvidia0=%q nvidia1=%q", devs.Health("nvidia0"), devs.Health("nvidia1"))
		}
	})
}

func TestRunnerMetrics(t *testing.T) {
	devs := newFakeDevices([]PhysicalGPU{"nvidia0"}, map[string]Health{"nvidia0": Healthy}, nil)
	mon := &fakeMonitors{running: true}
	nv := newFlowNVML(map[PhysicalGPU]string{"nvidia0": "0000:05:00.0"}, nil)
	clk := newStepClock(time.Date(2026, 11, 3, 1, 0, 0, 0, time.UTC), 2*time.Second)

	r := newTestFlowRunner(t, Config{}, NewMode(Config{}, WithNVML(nv)), devs, mon, nv, nil)
	r.now = clk.Now

	reg := prometheus.NewRegistry()
	if err := r.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if s := r.Run(context.Background(), "0"); s.LastRequest.OverallStatus != CodeSucceeded {
		t.Fatalf("Run 1 status = %q", s.LastRequest.OverallStatus)
	}
	if s := r.Run(context.Background(), "99"); s.LastRequest.OverallStatus != CodeInvalidTarget {
		t.Fatalf("Run 2 status = %q", s.LastRequest.OverallStatus)
	}

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("reg.Gather: %v", err)
	}
	outcomes := make(map[string]float64)
	phases := make(map[string]uint64)
	for _, mf := range mfs {
		switch mf.GetName() {
		case "gpu_reset_outcomes_total":
			for _, m := range mf.GetMetric() {
				for _, lp := range m.GetLabel() {
					if lp.GetName() == "code" {
						outcomes[lp.GetValue()] = m.GetCounter().GetValue()
					}
				}
			}
		case "gpu_reset_phase_duration_seconds":
			for _, m := range mf.GetMetric() {
				for _, lp := range m.GetLabel() {
					if lp.GetName() == "phase" {
						phases[lp.GetValue()] = m.GetHistogram().GetSampleCount()
					}
				}
			}
		}
	}
	if outcomes[string(CodeSucceeded)] != 1 || outcomes[string(CodeInvalidTarget)] != 1 || phases[string(PhaseValidate)] != 2 {
		t.Errorf("unexpected metrics: outcomes=%v phases=%v", outcomes, phases)
	}
}
