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

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"reflect"
	"testing"
	"time"

	gpumanager "github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia"
	"github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia/gpusharing"
	"github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia/metrics"
	"github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia/reset"
	"github.com/prometheus/client_golang/prometheus"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

func TestEnableGPUResetFlagDefaultFalse(t *testing.T) {
	f := flag.Lookup("enable-gpu-reset")
	if f == nil {
		t.Fatal("--enable-gpu-reset flag is not registered")
	}
	if f.DefValue != "false" {
		t.Errorf("--enable-gpu-reset default = %q, want %q", f.DefValue, "false")
	}
}

func TestResetConfigFromGPUConfig(t *testing.T) {
	gpuCfg := gpumanager.GPUConfig{
		GPUPartitionSize: "1g.5gb",
		GPUSharingConfig: gpumanager.GPUSharingConfig{
			GPUSharingStrategy:     gpusharing.MPS,
			MaxSharedClientsPerGPU: 8,
		},
		GPUFractionDivisor: 1,
	}
	got := resetConfigFromGPUConfig(gpuCfg)
	want := reset.Config{
		PartitionSize:          "1g.5gb",
		Sharing:                gpusharing.MPS,
		MaxSharedClientsPerGPU: 8,
		FractionDivisor:        1,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("resetConfigFromGPUConfig = %+v, want %+v", got, want)
	}

	// Legacy MaxTimeSharedClientsPerGPU > 0 overrides GPUSharingConfig (matching manager.go:94).
	legacyCfg := gpumanager.GPUConfig{
		MaxTimeSharedClientsPerGPU: 4,
		GPUSharingConfig: gpumanager.GPUSharingConfig{
			GPUSharingStrategy:     gpusharing.MPS,
			MaxSharedClientsPerGPU: 8,
		},
	}
	gotLegacy := resetConfigFromGPUConfig(legacyCfg)
	wantLegacy := reset.Config{
		Sharing:                gpusharing.TimeSharing,
		MaxSharedClientsPerGPU: 4,
	}
	if !reflect.DeepEqual(gotLegacy, wantLegacy) {
		t.Errorf("resetConfigFromGPUConfig (legacy override) = %+v, want %+v", gotLegacy, wantLegacy)
	}
}

func TestNGMDevices_SetHealthConsumptionAndTimeoutError(t *testing.T) {
	ngm := gpumanager.NewNvidiaGPUManager(t.TempDir(), t.TempDir(), nil, gpumanager.GPUConfig{})
	topo := &pluginapi.TopologyInfo{Nodes: []*pluginapi.NUMANode{{ID: 1}}}
	ngm.SetDeviceHealth("nvidia1", pluginapi.Healthy, nil)
	ngm.SetDeviceHealth("nvidia0", pluginapi.Healthy, topo)

	adapter := newNGMDevices(ngm, ngm.Health)
	adapter.sendTimeout = 20 * time.Millisecond

	wantPhys := []reset.PhysicalGPU{"nvidia0", "nvidia1"}
	if got := adapter.Physical(); !reflect.DeepEqual(got, wantPhys) {
		t.Fatalf("Physical() = %v, want %v", got, wantPhys)
	}
	if got := adapter.Health("nvidia0"); got != reset.Healthy {
		t.Fatalf("Health(nvidia0) = %q, want %q", got, reset.Healthy)
	}

	// 1. When ListAndWatch is reading from ngm.Health, SetHealth delivers on ngm.Health and returns nil.
	received := make(chan pluginapi.Device, 1)
	go func() {
		received <- <-ngm.Health
	}()
	if err := adapter.SetHealth("nvidia0", reset.Unhealthy); err != nil {
		t.Fatalf("SetHealth(nvidia0, Unhealthy) unexpected error: %v", err)
	}
	select {
	case dev := <-received:
		if dev.ID != "nvidia0" || dev.Health != pluginapi.Unhealthy {
			t.Errorf("received device = %+v, want nvidia0 Unhealthy", dev)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for health update on ngm.Health")
	}
	if got := adapter.Health("nvidia0"); got != reset.Unhealthy {
		t.Errorf("Health(nvidia0) after SetHealth = %q, want %q", got, reset.Unhealthy)
	}

	// 2. When ListAndWatch does NOT consume ngm.Health within sendTimeout, SetHealth
	// updates ngm's in-memory device table first and returns an error on timeout.
	if err := adapter.SetHealth("nvidia0", reset.Healthy); err == nil {
		t.Fatal("expected error when ngm.Health send times out")
	}
	if got := adapter.Health("nvidia0"); got != reset.Healthy {
		t.Errorf("Health(nvidia0) after timed-out SetHealth = %q, want %q", got, reset.Healthy)
	}
	// Verify topology was preserved.
	if gotTopo := ngm.ListPhysicalGPUs()["nvidia0"].Topology; !reflect.DeepEqual(gotTopo, topo) {
		t.Errorf("Topology after SetHealth = %+v, want %+v", gotTopo, topo)
	}

	// 3. When MarkUnhealthy send fails on ngmDevices, Runner.Run aborts and unwinds the device table back to Healthy.
	runner := reset.NewRunner(reset.Config{}, reset.NewMode(reset.Config{}), adapter, nil, stubNVML{})
	st := runner.Run(context.Background(), "0")
	if st.LastRequest.OverallStatus != reset.CodeReconcileFailed {
		t.Errorf("Runner.Run OverallStatus on MarkUnhealthy send timeout = %q, want %q", st.LastRequest.OverallStatus, reset.CodeReconcileFailed)
	}
	if got := ngm.DeviceHealth("nvidia0"); got != pluginapi.Healthy {
		t.Errorf("ngm.DeviceHealth(nvidia0) after MarkUnhealthy abort = %q, want %q", got, pluginapi.Healthy)
	}
}

type fakeMonitor struct {
	name      string
	ops       *[]string
	startErr  error
	refreshed map[string]pluginapi.Device
}

func (f *fakeMonitor) Stop() {
	*f.ops = append(*f.ops, f.name+".Stop")
}

func (f *fakeMonitor) Start() error {
	*f.ops = append(*f.ops, f.name+".Start")
	return f.startErr
}

func (f *fakeMonitor) RefreshDevices(devices map[string]pluginapi.Device) {
	*f.ops = append(*f.ops, f.name+".RefreshDevices")
	f.refreshed = devices
}

func TestPluginMonitors_StopStartAndNilSafety(t *testing.T) {
	// Typed-nil concrete pointers must not panic on Stop, Start, or RefreshDevices.
	nilMonitors := newPluginMonitors(nil, nil, nil)
	nilMonitors.Stop()
	nilMonitors.RefreshDevices()
	if err := nilMonitors.Start(); err != nil {
		t.Fatalf("nilMonitors.Start() unexpected error: %v", err)
	}

	ngm := gpumanager.NewNvidiaGPUManager(t.TempDir(), t.TempDir(), nil, gpumanager.GPUConfig{GPUPartitionSize: "1g.5gb"})
	ngm.SetDeviceHealth("nvidia0/gi2", pluginapi.Healthy, nil)

	var ops []string
	hcMon := &fakeMonitor{name: "hc", ops: &ops}
	m := &pluginMonitors{
		deviceLister:  ngm.ListPhysicalDevices,
		healthChecker: hcMon,
		metricServer:  &fakeMonitor{name: "ms", ops: &ops},
	}
	m.Stop()
	if err := m.Start(); err != nil {
		t.Fatalf("Start() unexpected error: %v", err)
	}
	wantOps := []string{"hc.Stop", "ms.Stop", "hc.RefreshDevices", "hc.Start", "ms.Start"}
	if !reflect.DeepEqual(ops, wantOps) {
		t.Errorf("monitor ops = %v, want %v", ops, wantOps)
	}
	if _, ok := hcMon.refreshed["nvidia0/gi2"]; !ok {
		t.Errorf("expected refreshed devices to include nvidia0/gi2, got %v", hcMon.refreshed)
	}

	// Verify both Start() methods are called even if the first fails.
	ops = nil
	mErr := &pluginMonitors{
		healthChecker: &fakeMonitor{name: "hc", ops: &ops, startErr: errors.New("hc boom")},
		metricServer:  &fakeMonitor{name: "ms", ops: &ops},
	}
	if err := mErr.Start(); err == nil {
		t.Fatal("expected error from Start when healthChecker fails")
	}
	if !reflect.DeepEqual(ops, []string{"hc.Start", "ms.Start"}) {
		t.Errorf("ops on partial error = %v, want [hc.Start ms.Start]", ops)
	}
}

type stubNVML struct{}

func (stubNVML) Init() error                                 { return nil }
func (stubNVML) Shutdown() error                             { return nil }
func (stubNVML) PCIBusID(reset.PhysicalGPU) (string, error)  { return "0000:00:04.0", nil }
func (stubNVML) Processes(reset.PhysicalGPU) ([]int, error)  { return nil, nil }
func (stubNVML) FabricReady(reset.PhysicalGPU) (bool, error) { return true, nil }
func (stubNVML) Persistence(reset.PhysicalGPU) (bool, error) { return true, nil }
func (stubNVML) SetPersistence(reset.PhysicalGPU, bool) error {
	return nil
}

func TestNewResetWatcher_ValidationAndStartupIntegration(t *testing.T) {
	ngm := gpumanager.NewNvidiaGPUManager(t.TempDir(), t.TempDir(), nil, gpumanager.GPUConfig{})
	ngm.SetDeviceHealth("nvidia0", pluginapi.Unhealthy, nil)

	if _, _, err := newResetWatcher(nil, "node-1", gpumanager.GPUConfig{}, ngm, ngm.Health, nil, nil, stubNVML{}); err == nil {
		t.Error("expected error when kubeClient is nil")
	}
	cs := fake.NewSimpleClientset()
	if _, _, err := newResetWatcher(cs, "", gpumanager.GPUConfig{}, ngm, ngm.Health, nil, nil, stubNVML{}); err == nil {
		t.Error("expected error when nodeName is empty")
	}

	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-1",
			Labels: map[string]string{
				reset.TriggerLabel: "0",
			},
		},
	}
	cs = fake.NewSimpleClientset(node)
	ms := metrics.NewMetricServer(1000, 0, "/metrics")
	reg := prometheus.NewRegistry()

	watcher, _, err := newResetWatcher(cs, "node-1", gpumanager.GPUConfig{}, ngm, ngm.Health, newPluginMonitors(ngm, nil, ms), reg, stubNVML{})
	if err != nil {
		t.Fatalf("newResetWatcher: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Consume ngm.Health in the background so SetHealth returns immediately.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ngm.Health:
			}
		}
	}()

	doneCh := make(chan error, 1)
	go func() {
		doneCh <- watcher.Start(ctx)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		gotNode, err := cs.CoreV1().Nodes().Get(context.Background(), "node-1", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if raw := gotNode.Annotations[reset.StatusDetailsAnnotation]; raw != "" {
			var st reset.Status
			if err := json.Unmarshal([]byte(raw), &st); err != nil {
				t.Fatal(err)
			}
			if st.LastRequest.OverallStatus == reset.CodeInterrupted {
				if gotLbl := gotNode.Labels[reset.StatusLabel]; gotLbl != string(reset.CodeInterrupted) {
					t.Fatalf("node label %s = %q, want %q", reset.StatusLabel, gotLbl, reset.CodeInterrupted)
				}
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for watcher to record CodeInterrupted")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	if err := <-doneCh; err != nil {
		t.Fatalf("watcher.Start: %v", err)
	}

	// Startup crash recovery converged noopMode on nvidia0 and marked it Healthy.
	if got := ngm.DeviceHealth("nvidia0"); got != pluginapi.Healthy {
		t.Errorf("ngm.DeviceHealth(nvidia0) = %q, want %q", got, pluginapi.Healthy)
	}
}
