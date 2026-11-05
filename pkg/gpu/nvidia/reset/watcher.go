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
	"encoding/json"
	"fmt"
	"maps"
	"math/rand"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/golang/glog"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	clientv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
)

const (
	eventSource                                       = "nvidia-gpu-device-plugin"
	ReasonGPUResetSucceeded, ReasonGPUResetFailed     = "GPUResetSucceeded", "GPUResetFailed"
	ReasonGPUResetRejected, ReasonGPUResetInterrupted = "GPUResetRejected", "GPUResetInterrupted"
)

type (
	kubeNodeWriter struct {
		client      kubernetes.Interface
		nodeName    string
		broadcaster record.EventBroadcaster
		recorder    record.EventRecorder
	}
	Watcher struct {
		client   kubernetes.Interface
		nodeName string
		runner   *Runner
		node     NodeWriter
		now      func() time.Time
		sleep    func(context.Context, time.Duration) error
		mu       sync.Mutex
		wg       sync.WaitGroup
	}
	nodeTrigger struct {
		kind       TriggerKind
		raw        string
		bothLabels bool
	}
)

func NewNodeWriter(client kubernetes.Interface, nodeName string) NodeWriter {
	b := record.NewBroadcaster()
	b.StartLogging(glog.Infof)
	if client != nil {
		b.StartRecordingToSink(&clientv1.EventSinkImpl{Interface: client.CoreV1().Events("")})
	}
	return &kubeNodeWriter{
		client: client, nodeName: nodeName, broadcaster: b,
		recorder: b.NewRecorder(scheme.Scheme, v1.EventSource{Component: eventSource, Host: nodeName}),
	}
}
func (w *kubeNodeWriter) WriteStatus(ctx context.Context, s Status) error {
	lbl := string(s.LastRequest.OverallStatus)
	if errs := validation.IsValidLabelValue(lbl); lbl == "" || len(errs) > 0 {
		return fmt.Errorf("invalid %s label value %q: %s", StatusLabel, lbl, strings.Join(errs, "; "))
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"labels":      map[string]string{StatusLabel: lbl},
		"annotations": map[string]string{StatusDetailsAnnotation: string(b)},
	}})
	if err != nil {
		return err
	}
	_, err = w.client.CoreV1().Nodes().Patch(ctx, w.nodeName, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}
func (w *kubeNodeWriter) ClearTrigger(ctx context.Context) (string, error) {
	var prev string
	if n, err := w.client.CoreV1().Nodes().Get(ctx, w.nodeName, metav1.GetOptions{}); err == nil && n != nil {
		prev = cmp.Or(n.Labels[TriggerLabel], n.Labels[ForcedResetLabel])
	}
	patch := []byte(fmt.Sprintf(`{"metadata":{"labels":{%q:null,%q:null}}}`, TriggerLabel, ForcedResetLabel))
	_, err := w.client.CoreV1().Nodes().Patch(ctx, w.nodeName, types.MergePatchType, patch, metav1.PatchOptions{})
	return prev, err
}
func (w *kubeNodeWriter) Event(eventType, reason, message string) {
	if w.recorder != nil {
		w.recorder.Event(&v1.ObjectReference{Kind: "Node", Name: w.nodeName, UID: types.UID(w.nodeName)}, eventType, reason, message)
	}
}
func NewWatcher(client kubernetes.Interface, nodeName string, runner *Runner, node NodeWriter) *Watcher {
	if node == nil && client != nil {
		node = NewNodeWriter(client, nodeName)
	}
	return &Watcher{client: client, nodeName: nodeName, runner: runner, node: node, now: time.Now, sleep: realSleep}
}
func (w *Watcher) Start(ctx context.Context) error {
	if w.client == nil || w.nodeName == "" {
		return fmt.Errorf("watcher requires non-nil client and nodeName")
	}
	defer func() {
		if knw, ok := w.node.(*kubeNodeWriter); ok && knw.broadcaster != nil {
			knw.broadcaster.Shutdown()
		}
	}()
	defer w.wg.Wait()
	var lastNode, prevNode *v1.Node
	for backoff := time.Second; ctx.Err() == nil; {
		n, err := w.client.CoreV1().Nodes().Get(ctx, w.nodeName, metav1.GetOptions{})
		if err != nil {
			_ = w.sleep(ctx, backoff)
			backoff = min(backoff*2, time.Minute)
			continue
		}
		backoff, prevNode, lastNode = time.Second, lastNode, n.DeepCopy()
		if prevNode == nil {
			w.onAdd(ctx, n)
			lastNode.Labels = nil
		} else {
			w.onUpdate(ctx, prevNode, n)
		}
		timeoutSec := int64(300 + rand.Intn(300))
		watcher, err := w.client.CoreV1().Nodes().Watch(ctx, metav1.ListOptions{
			FieldSelector:   fields.OneTermEqualSelector("metadata.name", w.nodeName).String(),
			ResourceVersion: n.ResourceVersion, AllowWatchBookmarks: true, TimeoutSeconds: &timeoutSec,
		})
		if err != nil {
			_ = w.sleep(ctx, time.Second)
			continue
		}
		var watchErr error
		for active := true; active; {
			select {
			case <-ctx.Done():
				active = false
			case ev, ok := <-watcher.ResultChan():
				if !ok || ev.Type == watch.Error {
					if ok {
						watchErr = apierrors.FromObject(ev.Object)
					}
					active = false
				} else if wn, ok := ev.Object.(*v1.Node); ok && (ev.Type == watch.Added || ev.Type == watch.Modified) {
					w.onUpdate(ctx, lastNode, wn)
					lastNode = wn.DeepCopy()
				}
			}
		}
		watcher.Stop()
		if watchErr != nil && !apierrors.IsResourceExpired(watchErr) && !apierrors.IsGone(watchErr) {
			_ = w.sleep(ctx, time.Second)
		}
	}
	return nil
}
func extractTrigger(labels map[string]string) (nodeTrigger, bool) {
	if g, f := labels[TriggerLabel], labels[ForcedResetLabel]; f != "" {
		return nodeTrigger{kind: KindForced, raw: f, bothLabels: g != ""}, true
	} else if g != "" {
		return nodeTrigger{kind: KindGraceful, raw: g}, true
	}
	return nodeTrigger{}, false
}
func (w *Watcher) onAdd(ctx context.Context, n *v1.Node) {
	trig, hasTrigger := extractTrigger(n.Labels)
	if trig.bothLabels {
		w.handleTrigger(ctx, trig, false)
		return
	}
	var prev Status
	_ = json.Unmarshal([]byte(n.Annotations[StatusDetailsAnnotation]), &prev)
	annInProg, lblInProg := prev.LastRequest.OverallStatus == CodeInProgress, n.Labels[StatusLabel] == string(CodeInProgress)
	if hasTrigger || annInProg || lblInProg {
		if !annInProg && (!hasTrigger || prev.LastRequest.Target != trig.raw) {
			prev = Status{}
		}
		if lblInProg {
			prev.LastRequest.OverallStatus = CodeInProgress
		}
		w.handleInterrupted(ctx, trig, prev)
	}
}
func (w *Watcher) onUpdate(ctx context.Context, oldNode, newNode *v1.Node) {
	trig, ok := extractTrigger(newNode.Labels)
	if !ok || (oldNode != nil && oldNode.Labels[TriggerLabel] == newNode.Labels[TriggerLabel] && oldNode.Labels[ForcedResetLabel] == newNode.Labels[ForcedResetLabel]) {
		return
	}
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		w.handleTrigger(ctx, trig, true)
	}()
}
func (w *Watcher) finalizeNode(ctx context.Context, status Status, activeTarget, otherKey, rejectFmt string) {
	if w.node == nil {
		return
	}
	cleanCtx := context.WithoutCancel(ctx)
	var rejected string
	if otherKey != "" && w.client != nil {
		if cur, err := w.client.CoreV1().Nodes().Get(cleanCtx, w.nodeName, metav1.GetOptions{}); err == nil && cur != nil {
			rejected = cur.Labels[otherKey]
		}
	}
	if prev, _ := w.node.ClearTrigger(cleanCtx); rejectFmt != "" {
		if prev != "" && prev != activeTarget {
			rejected = prev
		}
		if rejected != "" {
			status.LastRequest.RejectedRequest = rejected
			w.node.Event(v1.EventTypeWarning, ReasonGPUResetRejected, fmt.Sprintf(rejectFmt, rejected, activeTarget))
		}
	}
	_ = w.node.WriteStatus(cleanCtx, status)
	switch status.LastRequest.OverallStatus {
	case CodeInterrupted:
		w.node.Event(v1.EventTypeWarning, ReasonGPUResetInterrupted, status.LastRequest.Message)
	case CodeSucceeded:
		w.node.Event(v1.EventTypeNormal, ReasonGPUResetSucceeded, status.LastRequest.Message)
	default:
		w.node.Event(v1.EventTypeWarning, ReasonGPUResetFailed, fmt.Sprintf("GPU reset %q finished with %s: %s", activeTarget, status.LastRequest.OverallStatus, status.LastRequest.Message))
	}
}
func (w *Watcher) handleInterrupted(ctx context.Context, trig nodeTrigger, prev Status) {
	target, kind := cmp.Or(trig.raw, prev.LastRequest.Target), cmp.Or(trig.kind, prev.LastRequest.Kind, KindGraceful)
	if (target == "" && prev.LastRequest.OverallStatus != CodeInProgress && len(prev.Devices) == 0) || !w.mu.TryLock() {
		return
	}
	defer w.mu.Unlock()
	cleanCtx := context.WithoutCancel(ctx)
	msg := "plugin restarted while GPU reset was in progress; run was not resumed"
	var devices map[PhysicalGPU]DeviceStatus
	if r := w.runner; r != nil {
		gpus := slices.Collect(maps.Keys(prev.Devices))
		if len(gpus) == 0 && r.devices != nil {
			if target != "" {
				if parsed, err := ParseTarget(target, r.devices.Physical()); err == nil {
					gpus = parsed.GPUs
				}
			} else if prev.LastRequest.OverallStatus == CodeInProgress {
				gpus = r.devices.Physical()
			}
		}
		gpus = sortPhysicalGPUs(gpus)
		if len(gpus) > 0 {
			pci := make(map[PhysicalGPU]string, len(gpus))
			for _, gpu := range gpus {
				if pci[gpu] = prev.Devices[gpu].PCIBusID; pci[gpu] == "" {
					pci[gpu], _ = resolvePCIBusID(r.nvml, gpu)
				}
				if r.nvml != nil {
					_ = r.nvml.SetPersistence(gpu, true)
				}
			}
			health := Healthy
			if r.mode != nil {
				if err := r.mode.Converge(cleanCtx, gpus); err != nil {
					health, msg = Unhealthy, fmt.Sprintf("%s (recovery converge failed: %v)", msg, err)
				}
			}
			if health == Healthy {
				r.refreshMonitors()
			}
			nowMeta := metav1.NewTime(r.now())
			devices = make(map[PhysicalGPU]DeviceStatus, len(gpus))
			for _, gpu := range gpus {
				r.setGPUHealth(gpu, health)
				ds := prev.Devices[gpu]
				ds.PCIBusID, ds.Status, ds.Message, ds.LastUpdated = pci[gpu], CodeInterrupted, msg, nowMeta
				devices[gpu] = ds
			}
		}
		r.recordOutcome(CodeInterrupted)
	}
	status := Status{LastRequest: Request{Target: target, Kind: kind, Phase: PhaseDone, OverallStatus: CodeInterrupted, Message: msg, Timestamp: metav1.NewTime(w.now())}, Devices: devices}
	w.finalizeNode(ctx, status, target, "", "GPU reset request %q was rejected while clearing interrupted reset %q; re-apply the label to run it")
}
func (w *Watcher) handle(ctx context.Context, raw string) {
	w.handleTrigger(ctx, nodeTrigger{kind: KindGraceful, raw: raw}, false)
}
func (w *Watcher) handleTrigger(ctx context.Context, trig nodeTrigger, verifyFresh bool) {
	if trig.raw == "" || !w.mu.TryLock() {
		return
	}
	defer w.mu.Unlock()
	if verifyFresh && w.client != nil {
		if fresh, err := w.client.CoreV1().Nodes().Get(ctx, w.nodeName, metav1.GetOptions{}); err == nil && fresh != nil {
			if freshTrig, ok := extractTrigger(fresh.Labels); !ok || freshTrig != trig {
				return
			}
		}
	}
	w.runLocked(ctx, trig)
}
func (w *Watcher) runLocked(ctx context.Context, trig nodeTrigger) {
	startTime, kind := w.now(), cmp.Or(trig.kind, KindGraceful)
	status := Status{LastRequest: Request{Target: trig.raw, Kind: kind, Phase: PhaseValidate, OverallStatus: CodeInProgress, Timestamp: metav1.NewTime(startTime)}}
	defer func() {
		if rec := recover(); rec != nil {
			msg := fmt.Sprintf("panic during GPU reset: %v", rec)
			if r := w.runner; r != nil {
				if r.status.LastRequest.Target != trig.raw {
					r.status, r.phaseStart = status, startTime
				}
				code := r.status.LastRequest.OverallStatus
				if code == "" || code == CodeInProgress || code == CodeSucceeded {
					code = CodeReconcileFailed
				}
				status = r.fail(&Abort{Code: code, Message: msg}, code, "")
			} else {
				status.LastRequest.OverallStatus, status.LastRequest.Message = CodeReconcileFailed, msg
			}
		}
		var otherKey, rejectFmt string
		if !trig.bothLabels {
			otherKey = map[TriggerKind]string{KindGraceful: ForcedResetLabel, KindForced: TriggerLabel}[kind]
			rejectFmt = "GPU reset request %q was rejected because reset %q was already in progress; re-apply the label after completion"
		}
		w.finalizeNode(ctx, status, trig.raw, otherKey, rejectFmt)
	}()
	if w.node != nil {
		_ = w.node.WriteStatus(ctx, status)
	}
	if trig.bothLabels {
		status.LastRequest.OverallStatus, status.LastRequest.Message = CodeInvalidTarget, fmt.Sprintf("only one of %s or %s may be set on a node at a time", TriggerLabel, ForcedResetLabel)
		if w.runner != nil {
			w.runner.recordOutcome(CodeInvalidTarget)
		}
	} else if w.runner == nil {
		status.LastRequest.OverallStatus, status.LastRequest.Message = CodeReconcileFailed, "GPU reset runner is nil"
	} else {
		status = w.runner.runWithKind(ctx, kind, trig.raw)
		return
	}
	status.LastRequest.Duration = metav1.Duration{Duration: w.now().Sub(startTime)}
}
