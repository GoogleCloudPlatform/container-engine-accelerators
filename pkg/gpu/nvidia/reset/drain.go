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
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia/metrics"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

const (
	idleTimeout, idleInterval, resetDeadline, fabricTimeout = 30 * time.Second, time.Second, 120 * time.Second, 60 * time.Second
	resetAttempts, defaultProcRoot, pfKthread               = 5, "/proc", 0x00200000
)

var resetBackoffs = [resetAttempts]time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second}

type (
	execExecutor struct{}
	nvmlAdapter  struct{}
	resetResult  struct {
		Attempts int
		InUse    bool
		Err      error
	}
)

func (execExecutor) Run(ctx context.Context, bin string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, bin, args...).CombinedOutput()
}
func realSleep(ctx context.Context, d time.Duration) error {
	if d > 0 {
		select {
		case <-ctx.Done():
		case <-time.After(d):
			return nil
		}
	}
	return ctx.Err()
}
func realJitter(base time.Duration) time.Duration {
	if maxJitter := base / 4; maxJitter > 0 {
		return time.Duration(rand.Int64N(int64(maxJitter))) + 1
	}
	return 0
}
func nvmlErr(op string, ret nvml.Return) error {
	switch ret {
	case nvml.SUCCESS:
		return nil
	case nvml.ERROR_NOT_SUPPORTED:
		return fmt.Errorf("nvml %s: not supported (ERROR_NOT_SUPPORTED)", op)
	case nvml.ERROR_NO_PERMISSION:
		return fmt.Errorf("nvml %s: permission denied (ERROR_NO_PERMISSION)", op)
	}
	if nvml.GetLibrary().Lookup("nvmlErrorString") == nil {
		return fmt.Errorf("nvml %s: %s", op, nvml.ErrorString(ret))
	}
	return fmt.Errorf("nvml %s: nvml.Return(%d)", op, ret)
}
func NewNVML() NVML                 { return nvmlAdapter{} }
func (nvmlAdapter) Init() error     { return nvmlErr("Init", nvml.Init()) }
func (nvmlAdapter) Shutdown() error { return nvmlErr("Shutdown", nvml.Shutdown()) }
func (nvmlAdapter) deviceByMinor(gpu PhysicalGPU) (nvml.Device, error) {
	targetMinor, err := parseMinor(gpu)
	if err != nil {
		return nvml.Device{}, err
	}
	count, ret := nvml.DeviceGetCount()
	if err := nvmlErr("DeviceGetCount", ret); err != nil {
		return nvml.Device{}, err
	}
	for i := 0; i < count; i++ {
		if dev, ret := nvml.DeviceGetHandleByIndex(i); ret == nvml.SUCCESS {
			if minor, ret := dev.GetMinorNumber(); ret == nvml.SUCCESS && minor == targetMinor {
				return dev, nil
			}
		}
	}
	return nvml.Device{}, fmt.Errorf("GPU %s (minor %d) not found among %d NVML devices", gpu, targetMinor, count)
}
func parseCString(buf []int8) string {
	var b []byte
	for _, v := range buf {
		b = append(b, byte(v))
	}
	return strings.TrimSpace(strings.SplitN(string(b), "\x00", 2)[0])
}
func (a nvmlAdapter) PCIBusID(gpu PhysicalGPU) (string, error) {
	dev, err := a.deviceByMinor(gpu)
	if err != nil {
		return "", err
	}
	pciInfo, ret := dev.GetPciInfo()
	if busID := cmp.Or(parseCString(pciInfo.BusId[:]), parseCString(pciInfo.BusIdLegacy[:])); ret == nvml.SUCCESS && busID != "" {
		return busID, nil
	}
	return "", cmp.Or(nvmlErr("GetPciInfo", ret), fmt.Errorf("nvml GetPciInfo(%s): empty PCI bus ID", gpu))
}
func (a nvmlAdapter) Processes(gpu PhysicalGPU) ([]int, error) {
	dev, err := a.deviceByMinor(gpu)
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, fn := range []func() ([]nvml.ProcessInfo, nvml.Return){dev.GetComputeRunningProcesses, dev.GetGraphicsRunningProcesses, dev.GetMPSComputeRunningProcesses} {
		infos, ret := fn()
		if ret != nvml.SUCCESS && ret != nvml.ERROR_NOT_SUPPORTED && ret != nvml.ERROR_FUNCTION_NOT_FOUND {
			return nil, nvmlErr("Processes", ret)
		}
		for _, info := range infos {
			if p := int(info.Pid); p > 0 && !slices.Contains(pids, p) {
				pids = append(pids, p)
			}
		}
	}
	slices.Sort(pids)
	return pids, nil
}
func evalFabric(gpu PhysicalGPU, info nvml.GpuFabricInfo, ret nvml.Return) (bool, error) {
	switch {
	case ret == nvml.ERROR_NOT_SUPPORTED || ret == nvml.ERROR_FUNCTION_NOT_FOUND || (ret == nvml.SUCCESS && info.State == nvml.GPU_FABRIC_STATE_NOT_SUPPORTED):
		return true, nil
	case ret != nvml.SUCCESS:
		return false, nvmlErr("GetGpuFabricInfo", ret)
	case info.State == nvml.GPU_FABRIC_STATE_NOT_STARTED || info.State == nvml.GPU_FABRIC_STATE_IN_PROGRESS:
		return false, nil
	case info.State == nvml.GPU_FABRIC_STATE_COMPLETED:
		return nvml.Return(info.Status) == nvml.SUCCESS, nvmlErr("FabricStatus", nvml.Return(info.Status))
	default:
		return false, fmt.Errorf("GPU %s unexpected fabric state %d", gpu, info.State)
	}
}
func (a nvmlAdapter) FabricReady(gpu PhysicalGPU) (bool, error) {
	dev, err := a.deviceByMinor(gpu)
	if err != nil || nvml.GetLibrary().Lookup("nvmlDeviceGetGpuFabricInfo") != nil {
		return err == nil, err
	}
	info, ret := dev.GetGpuFabricInfo()
	return evalFabric(gpu, info, ret)
}
func (a nvmlAdapter) IsAmpere(gpu PhysicalGPU) (bool, error) {
	dev, err := a.deviceByMinor(gpu)
	if err != nil {
		return false, err
	}
	arch, ret := dev.GetArchitecture()
	return arch == nvml.DEVICE_ARCH_AMPERE, nvmlErr("GetArchitecture", ret)
}
func (a nvmlAdapter) Persistence(gpu PhysicalGPU) (bool, error) {
	dev, err := a.deviceByMinor(gpu)
	if err != nil {
		return false, err
	}
	state, ret := dev.GetPersistenceMode()
	return state == nvml.FEATURE_ENABLED, nvmlErr("GetPersistenceMode("+string(gpu)+")", ret)
}
func (a nvmlAdapter) SetPersistence(gpu PhysicalGPU, enabled bool) error {
	dev, err := a.deviceByMinor(gpu)
	if err != nil {
		return err
	}
	mode := map[bool]nvml.EnableState{false: nvml.FEATURE_DISABLED, true: nvml.FEATURE_ENABLED}[enabled]
	return nvmlErr(fmt.Sprintf("SetPersistenceMode(%s,%t)", gpu, enabled), dev.SetPersistenceMode(mode))
}
func isAllowedProc(dir, comm string) bool {
	if exe, err := os.Readlink(filepath.Join(dir, "exe")); err == nil && exe != "" {
		return slices.Contains([]string{"nv-fabricmanager", "nvidia-cuda-mps-control", "nvidia-cuda-mps-server"}, filepath.Base(strings.TrimSuffix(exe, " (deleted)")))
	}
	return slices.Contains([]string{"nv-fabricmanager", "nv-fabricmanage", "nvidia-cuda-mps-control", "nvidia-cuda-mps-server", "nvidia-cuda-mps"}, comm)
}
func isKernelThread(procRoot string, pid int, comm string) bool {
	dir := filepath.Join(procRoot, strconv.Itoa(pid))
	b1, err1 := os.ReadFile(filepath.Join(dir, "status"))
	if pid == 2 || strings.Contains(string(b1), "Kthread:\t1") || strings.Contains(string(b1), "PPid:\t2\n") {
		return true
	}
	b2, err2 := os.ReadFile(filepath.Join(dir, "stat"))
	if i := strings.LastIndexByte(string(b2), ')'); err2 == nil && i >= 0 && i+2 < len(b2) {
		if f := strings.Fields(string(b2[i+2:])); len(f) > 6 {
			flags, _ := strconv.ParseUint(f[6], 10, 64)
			if f[1] == "2" || flags&pfKthread != 0 {
				return true
			}
		}
	}
	return err1 != nil && err2 != nil && strings.HasPrefix(comm, "[") && strings.HasSuffix(comm, "]")
}
func parseCgroupContent(raw string) string {
	var chosen string
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if line = strings.TrimSpace(line); strings.Contains(line, "kubepods") || strings.Contains(line, "pod") {
			chosen = line
			break
		} else if line != "" && (chosen == "" || strings.HasPrefix(line, "0::")) {
			chosen = line
		}
	}
	if parts := strings.SplitN(chosen, ":", 3); len(parts) == 3 {
		return strings.TrimSpace(parts[2])
	}
	return chosen
}
func cmpPID(a, b BlockingProcess) int { return cmp.Compare(a.PID, b.PID) }
func (r *Runner) inspectPID(pid int, prev BlockingProcess) (BlockingProcess, bool, bool) {
	pidDir := filepath.Join(r.procRoot, strconv.Itoa(pid))
	_, err1 := os.Stat(pidDir)
	bComm, _ := os.ReadFile(filepath.Join(pidDir, "comm"))
	bCg, _ := os.ReadFile(filepath.Join(pidDir, "cgroup"))
	_, err2 := os.Stat(pidDir)
	alive := err1 == nil && err2 == nil
	comm, cgroup := cmp.Or(strings.TrimSpace(string(bComm)), prev.Comm), cmp.Or(parseCgroupContent(string(bCg)), prev.Cgroup)
	ignore := pid <= 0 || pid == r.selfPID || isAllowedProc(pidDir, comm) || (alive && isKernelThread(r.procRoot, pid, comm))
	return BlockingProcess{PID: pid, Comm: comm, Pod: prev.Pod, Cgroup: cgroup}, ignore, alive
}
func (r *Runner) heldGPUs(pid int, devToGPU map[string]PhysicalGPU) (held []PhysicalGPU) {
	if pid <= 0 || pid == r.selfPID {
		return nil
	}
	entries, _ := os.ReadDir(filepath.Join(r.procRoot, strconv.Itoa(pid), "fd"))
	for _, entry := range entries {
		if link, err := os.Readlink(filepath.Join(r.procRoot, strconv.Itoa(pid), "fd", entry.Name())); err == nil {
			if gpu, ok := devToGPU[filepath.Clean(link)]; ok && !slices.Contains(held, gpu) {
				held = append(held, gpu)
			}
		}
	}
	return held
}
func devMap(gpus []PhysicalGPU) map[string]PhysicalGPU {
	m := make(map[string]PhysicalGPU, len(gpus))
	for _, gpu := range gpus {
		if minor, err := parseMinor(gpu); err == nil {
			m[fmt.Sprintf("/dev/nvidia%d", minor)] = gpu
		}
	}
	return m
}
func (r *Runner) busyAbort(prefix string, blocking map[PhysicalGPU][]BlockingProcess) error {
	var parts []string
	for _, gpu := range slices.Sorted(maps.Keys(blocking)) {
		slices.SortFunc(blocking[gpu], cmpPID)
		var descs []string
		for _, p := range blocking[gpu] {
			descs = append(descs, strings.TrimSpace(fmt.Sprintf("pid=%d comm=%s cgroup=%s", p.PID, p.Comm, p.Cgroup)))
		}
		s := fmt.Sprintf("%s held by [%s]", gpu, strings.Join(descs, ", "))
		if pods := r.allocatedPods[gpu]; len(pods) > 0 {
			s += fmt.Sprintf(" (allocatedPods=%s)", strings.Join(pods, ","))
		}
		parts = append(parts, s)
	}
	if len(parts) > 0 {
		prefix += ": " + strings.Join(parts, "; ")
	}
	return &Abort{Code: CodeGPUBusy, Blocking: blocking, Message: prefix}
}
func (r *Runner) pollUntil(ctx context.Context, deadline time.Time, step func() (bool, error), onTimeout func() error) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if done, err := step(); err != nil || done {
			return err
		}
		rem := deadline.Sub(r.now())
		if rem <= 0 {
			return onTimeout()
		}
		if err := r.sleep(ctx, min(idleInterval, rem)); err != nil {
			return err
		}
	}
}
func (r *Runner) scanNVML(gpu PhysicalGPU, candMap map[int]BlockingProcess, devToGPU map[string]PhysicalGPU) ([]BlockingProcess, error) {
	if r.nvml == nil {
		return nil, fmt.Errorf("NVML not configured")
	}
	pids, err := r.nvml.Processes(gpu)
	if err != nil {
		return nil, err
	}
	active := make(map[int]bool, len(pids))
	for _, pid := range pids {
		if pid > 0 && pid != r.selfPID {
			active[pid], candMap[pid] = true, cmp.Or(candMap[pid], BlockingProcess{PID: pid})
		}
	}
	var out []BlockingProcess
	for pid, prev := range candMap {
		if bp, ignore, _ := r.inspectPID(pid, prev); ignore || (!active[pid] && !slices.Contains(r.heldGPUs(pid, devToGPU), gpu)) {
			delete(candMap, pid)
		} else {
			candMap[pid], out = bp, append(out, bp)
		}
	}
	slices.SortFunc(out, cmpPID)
	return out, nil
}
func (r *Runner) attribute(ctx context.Context, gpus []PhysicalGPU) (map[PhysicalGPU][]BlockingProcess, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.allocatedPods = make(map[PhysicalGPU][]string, len(gpus))
	if podDevices, err := metrics.ListPodGPUDevices(ctx, r.podResourcesSocket); err == nil {
		for _, gpu := range gpus {
			ids := r.deviceIDs(gpu)
			var pods []string
			for podKey, devIDs := range podDevices {
				if slices.ContainsFunc(devIDs, func(d string) bool {
					return d == string(gpu) || strings.HasPrefix(d, string(gpu)+"/") || slices.Contains(ids, d)
				}) {
					pods = append(pods, podKey)
				}
			}
			slices.Sort(pods)
			r.allocatedPods[gpu] = pods
			r.setDevice(gpu, func(ds *DeviceStatus) { ds.AllocatedPods = pods })
		}
	}
	candidates := make(map[PhysicalGPU][]BlockingProcess, len(gpus))
	for _, gpu := range gpus {
		procs, err := r.scanNVML(gpu, make(map[int]BlockingProcess), nil)
		if err != nil {
			return nil, fmt.Errorf("list NVML processes for %s: %w", gpu, err)
		} else if len(procs) > 0 {
			candidates[gpu] = procs
		}
	}
	return candidates, ctx.Err()
}
func (r *Runner) waitForIdle(ctx context.Context, gpus []PhysicalGPU, candidates map[PhysicalGPU][]BlockingProcess) error {
	devToGPU, candByGPU := devMap(gpus), make(map[PhysicalGPU]map[int]BlockingProcess, len(gpus))
	for _, gpu := range gpus {
		candByGPU[gpu] = make(map[int]BlockingProcess)
		for _, bp := range candidates[gpu] {
			candByGPU[gpu][bp.PID] = bp
		}
	}
	var blocking map[PhysicalGPU][]BlockingProcess
	return r.pollUntil(ctx, cmp.Or(r.idleDeadline, r.now().Add(idleTimeout)), func() (bool, error) {
		blocking = make(map[PhysicalGPU][]BlockingProcess)
		for _, gpu := range gpus {
			procs, err := r.scanNVML(gpu, candByGPU[gpu], devToGPU)
			if err != nil {
				return false, fmt.Errorf("query NVML processes for %s: %w", gpu, err)
			} else if len(procs) > 0 {
				blocking[gpu] = procs
			}
		}
		return len(blocking) == 0, nil
	}, func() error { return r.busyAbort("timed out after 30s waiting for GPU processes to exit", blocking) })
}
func (r *Runner) gate(ctx context.Context, gpus []PhysicalGPU) error {
	devToGPU := devMap(gpus)
	if len(devToGPU) == 0 {
		return ctx.Err()
	}
	var blocking map[PhysicalGPU][]BlockingProcess
	return r.pollUntil(ctx, cmp.Or(r.idleDeadline, r.now().Add(idleTimeout)), func() (bool, error) {
		entries, err := os.ReadDir(r.procRoot)
		if err != nil {
			return false, fmt.Errorf("read proc directory %s: %w", r.procRoot, err)
		}
		blocking = make(map[PhysicalGPU][]BlockingProcess)
		for _, entry := range entries {
			pid, _ := strconv.Atoi(entry.Name())
			if held := r.heldGPUs(pid, devToGPU); len(held) > 0 {
				if bp, ignore, alive := r.inspectPID(pid, BlockingProcess{}); alive && !ignore {
					for _, gpu := range held {
						blocking[gpu] = append(blocking[gpu], bp)
					}
				}
			}
		}
		return len(blocking) == 0, nil
	}, func() error {
		return r.busyAbort("foreign or unclosed device file descriptors detected at gate", blocking)
	})
}
func isGPUInUse(out []byte, err error) bool {
	s := strings.ToLower(fmt.Sprintf("%s %v", out, err))
	var exitErr *exec.ExitError
	return (errors.As(err, &exitErr) && exitErr.ExitCode() == int(nvml.ERROR_IN_USE)) ||
		slices.ContainsFunc([]string{"in use by another process", "in use by another client", "currently in use", "error_in_use"}, func(p string) bool { return strings.Contains(s, p) })
}
func normBDF(s string) string {
	if s = strings.ToLower(strings.TrimSpace(s)); len(s) > 9 && s[8] == ':' && strings.HasPrefix(s, "0000") {
		return s[4:]
	}
	return s
}
func parseResetOutput(out []byte) (resetOK map[string]bool, reasons map[string]string) {
	resetOK, reasons = make(map[string]bool), make(map[string]string)
	for _, line := range strings.Split(string(out), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "GPU "); ok {
			if bdf, ok := strings.CutSuffix(rest, " was successfully reset."); ok {
				resetOK[normBDF(bdf)] = true
			} else if bdf, reason, ok := strings.Cut(rest, ": "); ok {
				reasons[normBDF(bdf)] = strings.TrimSpace(reason)
			}
		}
	}
	return resetOK, reasons
}

// resetGPUs resets target GPUs in a single `nvidia-smi --gpu-reset -i <bdf1>,...,<bdfN>` call per
// attempt under one 120s deadline (never running concurrent nvidia-smi processes, which deadlock in
// nv_pci_remove). Must run immediately after the /proc fd gate.
func (r *Runner) resetGPUs(ctx context.Context, pci map[PhysicalGPU]string) map[PhysicalGPU]resetResult {
	results := make(map[PhysicalGPU]resetResult, len(pci))
	var pending []PhysicalGPU
	for _, gpu := range sortPhysicalGPUs(slices.Collect(maps.Keys(pci))) {
		if bdf := strings.TrimSpace(pci[gpu]); bdf == "" || strings.EqualFold(bdf, "all") {
			results[gpu] = resetResult{Err: fmt.Errorf("resetGPUs requires a specific PCI bus ID for %s, got %q", gpu, pci[gpu])}
		} else {
			pending = append(pending, gpu)
		}
	}
	deadline := r.now().Add(resetDeadline)
	setPendingErr := func(fn func(PhysicalGPU) error) {
		for _, gpu := range pending {
			res := results[gpu]
			res.Err = cmp.Or(res.Err, ctx.Err(), fn(gpu))
			results[gpu] = res
		}
	}
	for attempt := 1; attempt <= resetAttempts && len(pending) > 0; attempt++ {
		if attempt > 1 {
			base := resetBackoffs[attempt-2]
			if d := max(base+r.jitter(base), base); ctx.Err() != nil || d > deadline.Sub(r.now()) || r.sleep(ctx, d) != nil {
				setPendingErr(func(PhysicalGPU) error { return nil })
				break
			}
		}
		if ctx.Err() != nil || deadline.Sub(r.now()) <= 0 {
			setPendingErr(func(g PhysicalGPU) error {
				return fmt.Errorf("reset deadline (%s) exceeded before resetting %s: %w", resetDeadline, g, context.DeadlineExceeded)
			})
			break
		}
		bdfs := make([]string, len(pending))
		for i, gpu := range pending {
			bdfs[i] = strings.TrimSpace(pci[gpu])
		}
		tCtx, tCancel := context.WithTimeout(ctx, deadline.Sub(r.now()))
		out, err := r.run.Run(tCtx, nvidiaSMI, "--gpu-reset", "-i", strings.Join(bdfs, ","))
		tCancel()
		msg, errText := strings.TrimSpace(string(out)), ""
		if err != nil {
			errText = err.Error()
		}
		resetOK, reasons := parseResetOutput(out)
		batchInUse, anyInUse := len(reasons) == 0 && isGPUInUse(out, err), false
		var nextPending []PhysicalGPU
		for _, gpu := range pending {
			bdf, key := strings.TrimSpace(pci[gpu]), normBDF(pci[gpu])
			if resetOK[key] {
				results[gpu] = resetResult{Attempts: attempt}
				continue
			}
			reason := reasons[key]
			inUse := batchInUse || (reason != "" && isGPUInUse([]byte(reason), nil))
			anyInUse = anyInUse || inUse
			detail := cmp.Or(reason, msg, errText, "GPU not present in nvidia-smi reset output")
			baseErr := cmp.Or(err, errors.New(detail))
			if inUse {
				results[gpu] = resetResult{Attempts: attempt, InUse: true, Err: fmt.Errorf("nvidia-smi --gpu-reset -i %s refused (GPU in use): %w (output: %s)", bdf, baseErr, detail)}
			} else {
				results[gpu] = resetResult{Attempts: attempt, Err: fmt.Errorf("nvidia-smi --gpu-reset -i %s (attempt %d/%d) failed: %w (output: %s)", bdf, attempt, resetAttempts, baseErr, detail)}
				nextPending = append(nextPending, gpu)
			}
		}
		if anyInUse {
			break
		}
		pending = nextPending
	}
	return results
}
func (r *Runner) waitFabric(ctx context.Context, gpus []PhysicalGPU) error {
	var pending []PhysicalGPU
	return r.pollUntil(ctx, r.now().Add(fabricTimeout), func() (bool, error) {
		if r.nvml == nil {
			return false, fmt.Errorf("NVML not configured")
		}
		pending = nil
		for _, gpu := range gpus {
			if ready, err := r.nvml.FabricReady(gpu); err != nil {
				return false, fmt.Errorf("check fabric readiness for %s: %w", gpu, err)
			} else if !ready {
				pending = append(pending, gpu)
			}
		}
		return len(pending) == 0, nil
	}, func() error {
		return fmt.Errorf("timed out after %s waiting for NVLink fabric training on %v", fabricTimeout, pending)
	})
}
