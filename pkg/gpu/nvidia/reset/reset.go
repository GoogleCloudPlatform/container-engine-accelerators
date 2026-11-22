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

// Package reset implements in-process NVIDIA GPU reset for the GKE device plugin.
package reset

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia/gpusharing"
	"github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia/metrics"
	"github.com/golang/glog"
	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

const (
	TriggerLabel, ForcedResetLabel, StatusLabel                                  = "cloud.google.com/perform-gpu-reset", "cloud.google.com/perform-forced-gpu-reset", "cloud.google.com/gpu-reset-status"
	StatusDetailsAnnotation, nvidiaSMI                                           = "cloud.google.com/gpu-reset-status-details", "/usr/local/nvidia/bin/nvidia-smi"
	KindGraceful, KindForced, Healthy, Unhealthy                                 = TriggerKind("graceful"), TriggerKind("forced"), Health(pluginapi.Healthy), Health(pluginapi.Unhealthy)
	CodeInProgress, CodeSucceeded, CodePartialFailure                            = Code("InProgress"), Code("Succeeded"), Code("PartialFailure")
	CodeInvalidTarget, CodeUnsupportedNode, CodeUnsupportedMode, CodeRequiresAll = Code("Error-Invalid-Target"), Code("Error-Unsupported-Node"), Code("Error-Unsupported-Mode"), Code("Error-Requires-All")
	CodeNVMLFailed, CodeGPUBusy, CodeHardwareFailure, CodeReconcileFailed        = Code("Error-NVML-Failed"), Code("Error-GPU-Busy"), Code("Error-Hardware-Failure"), Code("Error-Reconcile-Failed")
	CodeInterrupted, PhaseValidate, PhaseMarkUnhealthy, PhaseResolve             = Code("Error-Interrupted"), Phase("Validate"), Phase("MarkUnhealthy"), Phase("Resolve")
	PhaseAttribute, PhaseWaitForIdle, PhaseReleasePersistence, PhaseGate         = Phase("Attribute"), Phase("WaitForIdle"), Phase("ReleasePersistence"), Phase("Gate")
	PhasePreReset, PhaseReleaseGPUs, PhaseReset, PhaseRecover, PhaseConverge     = Phase("PreReset"), Phase("ReleaseGPUs"), Phase("Reset"), Phase("Recover"), Phase("Converge")
	PhaseDone                                                                    = Phase("Done")
)

type (
	TriggerKind string
	PhysicalGPU string
	Code        string
	Phase       string
	Health      string
	Target      struct {
		Raw  string
		All  bool
		GPUs []PhysicalGPU
	}
	Status struct {
		LastRequest Request                      `json:"lastRequest"`
		Devices     map[PhysicalGPU]DeviceStatus `json:"devices,omitempty"`
	}
	Request struct {
		Target          string          `json:"target"`
		Kind            TriggerKind     `json:"kind"`
		Phase           Phase           `json:"phase"`
		OverallStatus   Code            `json:"overallStatus"`
		Message         string          `json:"message,omitempty"`
		Duration        metav1.Duration `json:"duration"`
		Timestamp       metav1.Time     `json:"timestamp"`
		RejectedRequest string          `json:"rejectedRequest,omitempty"`
	}
	DeviceStatus struct {
		PCIBusID      string            `json:"pciBusId,omitempty"`
		Status        Code              `json:"status"`
		Message       string            `json:"message,omitempty"`
		AllocatedPods []string          `json:"allocatedPods,omitempty"`
		BlockingPIDs  []BlockingProcess `json:"blockingPIDs,omitempty"`
		Attempts      int               `json:"attempts,omitempty"`
		LastUpdated   metav1.Time       `json:"lastUpdated"`
	}
	BlockingProcess struct {
		PID    int    `json:"pid"`
		Comm   string `json:"comm"`
		Pod    string `json:"pod,omitempty"`
		Cgroup string `json:"cgroup,omitempty"`
	}
	Abort struct {
		Code     Code
		Message  string
		Blocking map[PhysicalGPU][]BlockingProcess
	}
	Config struct {
		PartitionSize                           string
		Sharing                                 gpusharing.GPUSharingStrategy
		MaxSharedClientsPerGPU, FractionDivisor int
	}
	Devices interface {
		Physical() []PhysicalGPU
		Health(id string) Health
		SetHealth(id string, h Health) error
	}
	Monitors interface {
		Stop()
		Start() error
	}
	NVML interface {
		Init() error
		Shutdown() error
		PCIBusID(gpu PhysicalGPU) (string, error)
		Processes(gpu PhysicalGPU) ([]int, error)
		FabricReady(gpu PhysicalGPU) (bool, error)
		Persistence(gpu PhysicalGPU) (bool, error)
		SetPersistence(gpu PhysicalGPU, enabled bool) error
	}
	NodeWriter interface {
		WriteStatus(ctx context.Context, s Status) error
		ClearTrigger(ctx context.Context) (string, error)
		Event(eventType, reason, message string)
	}
	executor interface {
		Run(ctx context.Context, bin string, args ...string) ([]byte, error)
	}
	Runner struct {
		cfg                                  Config
		mode                                 Mode
		devices                              Devices
		monitors                             Monitors
		nvml                                 NVML
		run                                  executor
		devDir, procRoot, podResourcesSocket string
		selfPID                              int
		now                                  func() time.Time
		sleep                                func(context.Context, time.Duration) error
		jitter                               func(time.Duration) time.Duration
		undo                                 []func()
		allocatedPods                        map[PhysicalGPU][]string
		priorPersistence                     map[PhysicalGPU]bool
		phaseStart, idleDeadline             time.Time
		status                               Status
		outcomes                             *prometheus.CounterVec
		durations                            *prometheus.HistogramVec
	}
)

func (a *Abort) Error() string { return fmt.Sprintf("%s: %s", a.Code, a.Message) }
func NewRunner(cfg Config, mode Mode, devices Devices, monitors Monitors, nvml NVML) *Runner {
	return &Runner{
		cfg: cfg, mode: mode, devices: devices, monitors: monitors, nvml: cmp.Or[NVML](nvml, NewNVML()), run: execExecutor{},
		devDir: "/dev", procRoot: defaultProcRoot, selfPID: os.Getpid(), podResourcesSocket: metrics.DefaultPodResourcesSocket,
		now: time.Now, sleep: realSleep, jitter: realJitter,
		outcomes:  prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gpu_reset_outcomes_total", Help: "GPU reset outcomes by code."}, []string{"code"}),
		durations: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "gpu_reset_phase_duration_seconds", Help: "GPU reset phase durations.", Buckets: prometheus.DefBuckets}, []string{"phase"}),
	}
}
func (r *Runner) Register(reg prometheus.Registerer) error {
	return errors.Join(reg.Register(r.outcomes), reg.Register(r.durations))
}
func (r *Runner) recordOutcome(c Code) { r.outcomes.WithLabelValues(string(c)).Inc() }
func (r *Runner) refreshMonitors() {
	if dr, ok := r.monitors.(interface{ RefreshDevices() }); ok {
		dr.RefreshDevices()
	}
}
func (r *Runner) setDevice(gpu PhysicalGPU, fn func(*DeviceStatus)) {
	if r.status.Devices == nil {
		r.status.Devices = make(map[PhysicalGPU]DeviceStatus)
	}
	ds := r.status.Devices[gpu]
	fn(&ds)
	r.status.Devices[gpu] = ds
}
func parseMinor(gpu PhysicalGPU) (int, error) {
	s, ok := strings.CutPrefix(string(gpu), "nvidia")
	if !ok || s == "" || (len(s) > 1 && s[0] == '0') || strings.TrimLeft(s, "0123456789") != "" {
		return -1, fmt.Errorf("invalid physical GPU name %q", gpu)
	}
	return strconv.Atoi(s)
}
func sortPhysicalGPUs(gpus []PhysicalGPU) []PhysicalGPU {
	slices.SortFunc(gpus, func(a, b PhysicalGPU) int {
		ma, _ := parseMinor(a)
		mb, _ := parseMinor(b)
		return cmp.Or(cmp.Compare(ma, mb), cmp.Compare(a, b))
	})
	return gpus
}
func ParseTarget(raw string, physical []PhysicalGPU) (Target, error) {
	physSet := make(map[PhysicalGPU]bool, len(physical))
	var uniquePhys, gpus []PhysicalGPU
	for _, g := range physical {
		if _, err := parseMinor(g); err == nil && !physSet[g] {
			physSet[g], uniquePhys = true, append(uniquePhys, g)
		}
	}
	if raw == "all" && len(uniquePhys) > 0 {
		return Target{Raw: raw, All: true, GPUs: sortPhysicalGPUs(uniquePhys)}, nil
	}
	seen := make(map[PhysicalGPU]bool)
	for _, seg := range strings.Split(raw, "-") {
		gpu := PhysicalGPU("nvidia" + seg)
		if raw == "" || seen[gpu] || !physSet[gpu] {
			return Target{}, &Abort{Code: CodeInvalidTarget, Message: fmt.Sprintf("invalid target %q", raw)}
		}
		seen[gpu], gpus = true, append(gpus, gpu)
	}
	return Target{Raw: raw, GPUs: sortPhysicalGPUs(gpus)}, nil
}
func (r *Runner) checkPlatform(t Target) error {
	if r.cfg.FractionDivisor > 1 {
		return &Abort{Code: CodeUnsupportedNode, Message: "GPU reset is not supported on vGPU (fractional GPU) machine types"}
	}
	if sw, _ := filepath.Glob(filepath.Join(r.devDir, "nvidia-nvswitch*")); len(sw) > 0 {
		if ap, ok := r.nvml.(interface {
			IsAmpere(PhysicalGPU) (bool, error)
		}); ok {
			for _, gpu := range t.GPUs {
				if amp, err := ap.IsAmpere(gpu); err != nil {
					return &Abort{Code: CodeNVMLFailed, Message: fmt.Sprintf("failed to query NVML architecture for %s: %v", gpu, err)}
				} else if amp {
					return &Abort{Code: CodeUnsupportedNode, Message: "GPU reset is not supported on A2 machine types"}
				}
			}
		}
	}
	if r.cfg.Sharing == gpusharing.MPS {
		return &Abort{Code: CodeUnsupportedMode, Message: "GPU reset is not supported on MPS nodes"}
	}
	return nil
}
func (r *Runner) deviceIDs(gpu PhysicalGPU) []string {
	if r.mode != nil {
		if ids := r.mode.DeviceIDs(gpu); len(ids) > 0 {
			return ids
		}
	}
	return []string{string(gpu)}
}
func (r *Runner) setGPUHealth(gpu PhysicalGPU, h Health) {
	if r.devices != nil {
		for _, id := range r.deviceIDs(gpu) {
			_ = r.devices.SetHealth(id, h)
		}
	}
}
func (r *Runner) pushUndo(fn func()) { r.undo = append(r.undo, fn) }
func (r *Runner) unwindUndo() {
	for i := len(r.undo) - 1; i >= 0; i-- {
		r.undo[i]()
	}
	r.undo = nil
}
func (r *Runner) enterPhase(next Phase) {
	now := r.now()
	if p := r.status.LastRequest.Phase; p != "" {
		elapsed := max(now.Sub(r.phaseStart), 0)
		r.durations.WithLabelValues(string(p)).Observe(elapsed.Seconds())
		glog.V(2).Infof("gpu reset phase: target=%q from=%s to=%s elapsed=%s", r.status.LastRequest.Target, p, cmp.Or(string(next), "end"), elapsed)
	}
	r.status.LastRequest.Phase, r.phaseStart = cmp.Or(next, r.status.LastRequest.Phase), now
}
func (r *Runner) finishRun() Status {
	r.enterPhase("")
	r.status.LastRequest.Duration = metav1.Duration{Duration: r.phaseStart.Sub(r.status.LastRequest.Timestamp.Time)}
	r.recordOutcome(r.status.LastRequest.OverallStatus)
	return r.status
}
func (r *Runner) fail(err error, defaultCode Code, contextMsg string) Status {
	var ab *Abort
	if !errors.As(err, &ab) || ab == nil {
		if contextMsg != "" {
			err = fmt.Errorf("%s: %w", contextMsg, err)
		}
		ab = &Abort{Code: defaultCode, Message: err.Error()}
	}
	ab.Code = cmp.Or(ab.Code, defaultCode, CodeReconcileFailed)
	r.unwindUndo()
	now := metav1.NewTime(r.now())
	r.status.LastRequest.OverallStatus, r.status.LastRequest.Message = ab.Code, ab.Message
	for gpu := range r.status.Devices {
		r.setDevice(gpu, func(ds *DeviceStatus) {
			ds.Status, ds.Message, ds.LastUpdated, ds.AllocatedPods = ab.Code, ab.Message, now, r.allocatedPods[gpu]
			if procs := ab.Blocking[gpu]; len(procs) > 0 {
				ds.BlockingPIDs = procs
			}
		})
	}
	return r.finishRun()
}
func (r *Runner) Run(ctx context.Context, raw string) Status {
	return r.runWithKind(ctx, KindGraceful, raw)
}
func (r *Runner) runWithKind(ctx context.Context, kind TriggerKind, raw string) Status {
	kind, startTime := cmp.Or(kind, KindGraceful), r.now()
	r.undo, r.allocatedPods, r.priorPersistence, r.phaseStart, r.idleDeadline = nil, nil, nil, startTime, time.Time{}
	r.status = Status{LastRequest: Request{Target: raw, Kind: kind, Phase: PhaseValidate, OverallStatus: CodeInProgress, Timestamp: metav1.NewTime(startTime)}}
	defer r.unwindUndo()
	var physical []PhysicalGPU
	if r.devices != nil {
		physical = r.devices.Physical()
	}
	target, err := ParseTarget(raw, physical)
	if err != nil {
		return r.fail(err, CodeInvalidTarget, "validate target")
	}
	if err := r.checkPlatform(target); err != nil {
		return r.fail(err, CodeUnsupportedNode, "check platform")
	}
	if kind == KindForced { // Remove to enable forced reset.
		return r.fail(&Abort{Code: CodeUnsupportedMode, Message: "forced GPU reset is not supported yet"}, "", "")
	}
	for _, gpu := range target.GPUs {
		r.setDevice(gpu, func(ds *DeviceStatus) { ds.Status, ds.LastUpdated = CodeInProgress, metav1.NewTime(r.now()) })
	}
	r.enterPhase(PhaseMarkUnhealthy)
	for _, gpu := range target.GPUs {
		if r.devices == nil {
			break
		}
		ids, prior := r.deviceIDs(gpu), make(map[string]Health)
		for _, id := range ids {
			prior[id] = r.devices.Health(id)
		}
		r.pushUndo(func() {
			for _, id := range ids {
				_ = r.devices.SetHealth(id, prior[id])
			}
		})
		for _, id := range ids {
			if err := r.devices.SetHealth(id, Unhealthy); err != nil {
				return r.fail(err, CodeReconcileFailed, "mark unhealthy "+id)
			}
		}
	}
	r.enterPhase(PhaseResolve)
	pci := make(map[PhysicalGPU]string, len(target.GPUs))
	for _, gpu := range target.GPUs {
		busID, err := resolvePCIBusID(r.nvml, gpu)
		if err != nil {
			return r.fail(err, CodeNVMLFailed, "")
		}
		pci[gpu] = busID
		r.setDevice(gpu, func(ds *DeviceStatus) { ds.PCIBusID, ds.LastUpdated = busID, metav1.NewTime(r.now()) })
	}
	r.enterPhase(PhaseAttribute)
	candidates, err := r.attribute(ctx, target.GPUs)
	if err != nil {
		return r.fail(err, CodeNVMLFailed, "attribute processes")
	}
	// TODO: When kind == KindForced, publish r.allocatedPods to the target-pods node annotation here so accelerator-operator evicts blocking pods before PhaseWaitForIdle.
	r.enterPhase(PhaseWaitForIdle)
	r.idleDeadline = r.now().Add(idleTimeout)
	if err := r.waitForIdle(ctx, target.GPUs, candidates); err != nil {
		return r.fail(err, CodeGPUBusy, "wait for target GPUs to become idle")
	}
	r.enterPhase(PhaseReleasePersistence)
	undoCtx := context.WithoutCancel(ctx)
	r.priorPersistence = make(map[PhysicalGPU]bool, len(target.GPUs))
	for _, gpu := range target.GPUs {
		on, err := r.nvml.Persistence(gpu)
		if err != nil {
			return r.fail(err, CodeNVMLFailed, "query persistence on "+string(gpu))
		}
		r.priorPersistence[gpu] = on
		if !on {
			continue
		}
		if err := r.nvml.SetPersistence(gpu, false); err != nil {
			return r.fail(err, CodeHardwareFailure, "disable persistence on "+string(gpu))
		}
		r.pushUndo(func() { _ = r.nvml.SetPersistence(gpu, true) })
	}
	// Gate /proc/*/fd immediately before PreReset/ReleaseGPUs/Reset with no slow operations in between:
	// any raw open /dev/nvidiaX fd causes nvidia.ko (nv_pci_remove) to sleep uninterruptibly in unbind.
	r.enterPhase(PhaseGate)
	if err := r.gate(ctx, target.GPUs); err != nil {
		return r.fail(err, CodeGPUBusy, "global /proc fd gate")
	}
	r.enterPhase(PhasePreReset)
	if r.mode != nil {
		r.pushUndo(func() {
			if r.mode.Converge(undoCtx, target.GPUs) == nil {
				r.refreshMonitors()
			}
		})
		if err := r.mode.PreReset(ctx, target.GPUs); err != nil {
			return r.fail(err, CodeGPUBusy, "pre-reset mode prepare")
		}
	}
	r.enterPhase(PhaseReleaseGPUs)
	if r.monitors != nil {
		r.monitors.Stop()
		r.pushUndo(func() { _ = r.monitors.Start() })
	}
	if err := r.nvml.Shutdown(); err != nil {
		return r.fail(err, CodeNVMLFailed, "shutdown NVML")
	}
	r.pushUndo(func() { _ = r.nvml.Init() })
	r.enterPhase(PhaseReset)
	results := r.resetGPUs(ctx, pci)
	var resetSucceeded []PhysicalGPU
	var firstInUseErr error
	anyInUse := false
	for _, gpu := range target.GPUs {
		res := results[gpu]
		r.setDevice(gpu, func(ds *DeviceStatus) {
			ds.Attempts, ds.LastUpdated, ds.AllocatedPods = res.Attempts, metav1.NewTime(r.now()), r.allocatedPods[gpu]
		})
		if res.InUse {
			anyInUse, firstInUseErr = true, cmp.Or(firstInUseErr, res.Err)
		} else if res.Err == nil {
			resetSucceeded = append(resetSucceeded, gpu)
		}
	}
	if len(resetSucceeded) == 0 && anyInUse {
		r.idleDeadline = r.now()
		var gateAb *Abort
		_ = errors.As(r.gate(undoCtx, target.GPUs), &gateAb)
		ab := &Abort{Code: CodeGPUBusy, Message: fmt.Sprintf("driver reported GPU in use: %v", firstInUseErr)}
		if gateAb != nil {
			ab.Blocking = gateAb.Blocking
		}
		return r.fail(ab, CodeGPUBusy, "")
	}
	r.undo = []func(){func() {
		if r.monitors != nil {
			_ = r.monitors.Start()
		}
	}}
	r.enterPhase(PhaseRecover)
	nvmlInitErr := r.nvml.Init()
	var fabricErr, monitorsErr, convErr error
	if nvmlInitErr == nil && len(resetSucceeded) > 0 {
		fabricErr = r.waitFabric(ctx, resetSucceeded)
	}
	r.undo = nil
	if r.monitors != nil {
		monitorsErr = r.monitors.Start()
	}
	r.enterPhase(PhaseConverge)
	pmErrs := make(map[PhysicalGPU]error, len(target.GPUs))
	if nvmlInitErr == nil {
		for _, gpu := range target.GPUs {
			if r.priorPersistence[gpu] {
				pmErrs[gpu] = r.nvml.SetPersistence(gpu, true)
			}
		}
	}
	toConverge := slices.DeleteFunc(slices.Clone(resetSucceeded), func(g PhysicalGPU) bool { return nvmlInitErr != nil || fabricErr != nil || pmErrs[g] != nil })
	if len(toConverge) > 0 && r.mode != nil {
		convErr = r.mode.Converge(ctx, toConverge)
	}
	if len(toConverge) > 0 && convErr == nil {
		r.refreshMonitors()
	}
	nowConv, succeeded, firstErr, counts := metav1.NewTime(r.now()), 0, "", make(map[Code]int)
	for _, gpu := range target.GPUs {
		res := results[gpu]
		code, msg, h := CodeSucceeded, "Reset succeeded and NVML verified healthy", Healthy
		switch {
		case res.InUse && res.Err != nil:
			code, msg = CodeGPUBusy, res.Err.Error()
		case res.InUse:
			code, msg = CodeGPUBusy, "driver reported GPU in use by another process"
		case res.Err != nil:
			code, msg = CodeHardwareFailure, res.Err.Error()
		case nvmlInitErr != nil:
			code, msg = CodeNVMLFailed, "NVML re-initialization failed: "+nvmlInitErr.Error()
		case fabricErr != nil:
			code, msg = CodeReconcileFailed, "NVLink fabric training failed: "+fabricErr.Error()
		case pmErrs[gpu] != nil:
			code, msg = CodeReconcileFailed, "failed to re-enable persistence mode: "+pmErrs[gpu].Error()
		case convErr != nil:
			code, msg = CodeReconcileFailed, "mode converge failed: "+convErr.Error()
		case monitorsErr != nil:
			code, msg = CodeReconcileFailed, "failed to restart monitors: "+monitorsErr.Error()
		}
		if code != CodeSucceeded {
			h, firstErr = Unhealthy, cmp.Or(firstErr, msg)
		} else {
			succeeded++
		}
		r.setGPUHealth(gpu, h)
		r.setDevice(gpu, func(ds *DeviceStatus) { ds.Status, ds.Message, ds.LastUpdated = code, msg, nowConv })
		counts[code]++
	}
	r.enterPhase(PhaseDone)
	req := &r.status.LastRequest
	switch {
	case succeeded == len(target.GPUs):
		req.OverallStatus, req.Message = CodeSucceeded, fmt.Sprintf("reset succeeded for %d GPU(s)", succeeded)
	case succeeded > 0:
		req.OverallStatus, req.Message = CodePartialFailure, fmt.Sprintf("%d of %d GPU(s) reset succeeded; %d failed (%s)", succeeded, len(target.GPUs), len(target.GPUs)-succeeded, firstErr)
	case counts[CodeReconcileFailed] > 0:
		req.OverallStatus, req.Message = CodeReconcileFailed, firstErr
	case counts[CodeNVMLFailed] > 0:
		req.OverallStatus, req.Message = CodeNVMLFailed, firstErr
	default:
		req.OverallStatus, req.Message = CodeHardwareFailure, firstErr
	}
	return r.finishRun()
}
