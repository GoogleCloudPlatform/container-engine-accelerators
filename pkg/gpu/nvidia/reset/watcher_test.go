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
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
)

func assertPersistedNodeStatus(t *testing.T, node *v1.Node, wantCode Code) Status {
	t.Helper()
	lbl := node.Labels[StatusLabel]
	if errs := validation.IsValidLabelValue(lbl); lbl == "" || len(errs) > 0 || strings.ContainsAny(lbl, "{}\"") {
		t.Fatalf("invalid %s label %q: errs=%v", StatusLabel, lbl, errs)
	}
	if lbl != string(wantCode) {
		t.Fatalf("node label %s = %q, want %q", StatusLabel, lbl, wantCode)
	}
	if _, hasOldAnn := node.Annotations[StatusLabel]; hasOldAnn {
		t.Fatalf("unexpected annotation at label key %s: %v", StatusLabel, node.Annotations)
	}
	var persisted Status
	if err := json.Unmarshal([]byte(node.Annotations[StatusDetailsAnnotation]), &persisted); err != nil {
		t.Fatalf("unmarshal %s: %v (raw=%q)", StatusDetailsAnnotation, err, node.Annotations[StatusDetailsAnnotation])
	}
	if persisted.LastRequest.OverallStatus != wantCode {
		t.Fatalf("persisted details OverallStatus = %q, want %q", persisted.LastRequest.OverallStatus, wantCode)
	}
	return persisted
}

type recordingNodeWriter struct {
	mu           sync.Mutex
	inner        NodeWriter
	statuses     []Status
	clearCalls   int
	events       []string
	onInProgress chan Status
	onFinal      chan Status
}

func newRecordingNodeWriter(t *testing.T, inner NodeWriter) *recordingNodeWriter {
	t.Helper()
	if knw, ok := inner.(*kubeNodeWriter); ok && knw.broadcaster != nil {
		t.Cleanup(knw.broadcaster.Shutdown)
	}
	return &recordingNodeWriter{
		inner:        inner,
		onInProgress: make(chan Status, 10),
		onFinal:      make(chan Status, 10),
	}
}

func (r *recordingNodeWriter) WriteStatus(ctx context.Context, s Status) error {
	r.mu.Lock()
	r.statuses = append(r.statuses, s)
	r.mu.Unlock()

	var err error
	if r.inner != nil {
		err = r.inner.WriteStatus(ctx, s)
	}
	ch := r.onFinal
	if s.LastRequest.OverallStatus == CodeInProgress {
		ch = r.onInProgress
	}
	select {
	case ch <- s:
	default:
	}
	return err
}

func (r *recordingNodeWriter) ClearTrigger(ctx context.Context) (string, error) {
	r.mu.Lock()
	r.clearCalls++
	r.mu.Unlock()
	if r.inner != nil {
		return r.inner.ClearTrigger(ctx)
	}
	return "", nil
}

func (r *recordingNodeWriter) Event(eventType, reason, message string) {
	r.mu.Lock()
	r.events = append(r.events, fmt.Sprintf("%s %s %s", eventType, reason, message))
	r.mu.Unlock()
	if r.inner != nil {
		r.inner.Event(eventType, reason, message)
	}
}

func (r *recordingNodeWriter) recordedStatuses() []Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Status(nil), r.statuses...)
}

func (r *recordingNodeWriter) recordedEvents() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func newTestWatcherRunner(t *testing.T, exec *fakeExecutor) *Runner {
	t.Helper()
	devs := newFakeDevices([]PhysicalGPU{"nvidia0", "nvidia1"}, nil, nil)
	mon := &fakeMonitors{running: true}
	nv := newFlowNVML(map[PhysicalGPU]string{
		"nvidia0": "0000:05:00.0",
		"nvidia1": "0000:06:00.0",
	}, nil)
	clk := newStepClock(time.Date(2026, 11, 3, 2, 0, 0, 0, time.UTC), time.Second)
	r := newTestFlowRunner(t, Config{}, NewMode(Config{}, WithNVML(nv)), devs, mon, nv, exec)
	r.now = clk.Now
	return r
}

func waitWatchEstablished(t *testing.T, cs *fake.Clientset) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, a := range cs.Actions() {
			if a.GetVerb() == "watch" && a.GetResource().Resource == "nodes" {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for node watch to be established")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestWatcher_StartupLabel_RecordedAsInterruptedAndCleared(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "gpu-node-1",
			Labels: map[string]string{
				TriggerLabel: "0-1",
				"other-key":  "keep-me",
			},
		},
	}
	cs := fake.NewSimpleClientset(node)

	var resetCalls atomic.Int32
	exec := &fakeExecutor{
		fn: func(ctx context.Context, bin string, args ...string) ([]byte, error) {
			if len(args) >= 1 && args[0] == "--gpu-reset" {
				resetCalls.Add(1)
			}
			return nil, nil
		},
	}
	runner := newTestWatcherRunner(t, exec)
	reg := prometheus.NewRegistry()
	if err := runner.Register(reg); err != nil {
		t.Fatal(err)
	}

	nw := newRecordingNodeWriter(t, NewNodeWriter(cs, "gpu-node-1"))
	w := NewWatcher(cs, "gpu-node-1", runner, nw)

	doneCh := make(chan error, 1)
	go func() { doneCh <- w.Start(ctx) }()

	select {
	case s := <-nw.onFinal:
		if s.LastRequest.OverallStatus != CodeInterrupted || s.LastRequest.Target != "0-1" {
			t.Fatalf("LastRequest = %+v, want CodeInterrupted on 0-1", s.LastRequest)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for startup CodeInterrupted status")
	}

	cancel()
	if err := <-doneCh; err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if got := resetCalls.Load(); got != 0 {
		t.Errorf("resetCalls = %d on startup label, want 0", got)
	}

	updatedNode, err := cs.CoreV1().Nodes().Get(context.Background(), "gpu-node-1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := updatedNode.Labels[TriggerLabel]; exists || updatedNode.Labels["other-key"] != "keep-me" {
		t.Errorf("unexpected labels: %v", updatedNode.Labels)
	}
	assertPersistedNodeStatus(t, updatedNode, CodeInterrupted)
}

func TestWatcher_UpdateTrigger_AndResyncIgnored(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "gpu-node-1",
			Labels: map[string]string{"kubernetes.io/hostname": "gpu-node-1"},
		},
	}
	cs := fake.NewSimpleClientset(node)

	var resetCalls atomic.Int32
	exec := &fakeExecutor{
		fn: func(ctx context.Context, bin string, args ...string) ([]byte, error) {
			if len(args) >= 1 && args[0] == "--gpu-reset" {
				resetCalls.Add(1)
			}
			return nil, nil
		},
	}
	runner := newTestWatcherRunner(t, exec)
	nw := newRecordingNodeWriter(t, NewNodeWriter(cs, "gpu-node-1"))
	w := NewWatcher(cs, "gpu-node-1", runner, nw)

	doneCh := make(chan error, 1)
	go func() { doneCh <- w.Start(ctx) }()
	waitWatchEstablished(t, cs)

	patch := []byte(fmt.Sprintf(`{"metadata":{"labels":{%q:"0"}}}`, TriggerLabel))
	if _, err := cs.CoreV1().Nodes().Patch(ctx, "gpu-node-1", types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		t.Fatal(err)
	}

	select {
	case inProg := <-nw.onInProgress:
		if inProg.LastRequest.OverallStatus != CodeInProgress {
			t.Errorf("initial status = %q, want %q", inProg.LastRequest.OverallStatus, CodeInProgress)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for InProgress status")
	}

	select {
	case final := <-nw.onFinal:
		if final.LastRequest.OverallStatus != CodeSucceeded || final.LastRequest.Kind != KindGraceful {
			t.Errorf("final = %+v, want Succeeded Graceful", final.LastRequest)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for final Succeeded status")
	}

	unrelatedPatch := []byte(`{"metadata":{"labels":{"unrelated-label":"true"},"annotations":{"unrelated":"true"}}}`)
	if _, err := cs.CoreV1().Nodes().Patch(ctx, "gpu-node-1", types.MergePatchType, unrelatedPatch, metav1.PatchOptions{}); err != nil {
		t.Fatal(err)
	}

	cancel()
	if err := <-doneCh; err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if got := resetCalls.Load(); got != 1 {
		t.Errorf("resetCalls = %d, want 1", got)
	}
	finalNode, err := cs.CoreV1().Nodes().Get(context.Background(), "gpu-node-1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertPersistedNodeStatus(t, finalNode, CodeSucceeded)
}

func TestWatcher_ConcurrentTrigger_RejectedWithEventAndStatus(t *testing.T) {
	ctx := context.Background()
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "gpu-node-1",
			Labels: map[string]string{TriggerLabel: "0"},
		},
	}
	cs := fake.NewSimpleClientset(node)

	inRun := make(chan struct{})
	releaseRun := make(chan struct{})
	var resetCalls atomic.Int32

	exec := &fakeExecutor{
		fn: func(ctx context.Context, bin string, args ...string) ([]byte, error) {
			if len(args) >= 1 && args[0] == "--gpu-reset" {
				resetCalls.Add(1)
				close(inRun)
				<-releaseRun
			}
			return nil, nil
		},
	}
	runner := newTestWatcherRunner(t, exec)
	nw := newRecordingNodeWriter(t, NewNodeWriter(cs, "gpu-node-1"))
	w := NewWatcher(cs, "gpu-node-1", runner, nw)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.handle(ctx, "0")
	}()
	<-inRun

	midNode, err := cs.CoreV1().Nodes().Get(ctx, "gpu-node-1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertPersistedNodeStatus(t, midNode, CodeInProgress)

	patch := []byte(fmt.Sprintf(`{"metadata":{"labels":{%q:"1"}}}`, TriggerLabel))
	if _, err := cs.CoreV1().Nodes().Patch(ctx, "gpu-node-1", types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		t.Fatal(err)
	}
	w.handle(ctx, "1")

	close(releaseRun)
	wg.Wait()

	if got := resetCalls.Load(); got != 1 {
		t.Fatalf("resetCalls = %d, want 1 (no queueing)", got)
	}
	statuses := nw.recordedStatuses()
	if len(statuses) != 2 || statuses[1].LastRequest.OverallStatus != CodeSucceeded || statuses[1].LastRequest.RejectedRequest != "1" {
		t.Fatalf("statuses = %+v, want InProgress + Succeeded with RejectedRequest=1", statuses)
	}
	endNode, err := cs.CoreV1().Nodes().Get(ctx, "gpu-node-1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := endNode.Labels[TriggerLabel]; exists {
		t.Errorf("TriggerLabel not cleared: %v", endNode.Labels)
	}
	if persisted := assertPersistedNodeStatus(t, endNode, CodeSucceeded); persisted.LastRequest.RejectedRequest != "1" {
		t.Errorf("persisted RejectedRequest = %q, want 1", persisted.LastRequest.RejectedRequest)
	}
	foundRejectedEvent := false
	for _, ev := range nw.recordedEvents() {
		if strings.Contains(ev, v1.EventTypeWarning+" "+ReasonGPUResetRejected) && strings.Contains(ev, `"1"`) {
			foundRejectedEvent = true
		}
	}
	if !foundRejectedEvent {
		t.Errorf("expected Warning %s event in %v", ReasonGPUResetRejected, nw.recordedEvents())
	}
}

func TestWatcher_RunnerPanic_RecoveredStatusWrittenAndLabelCleared(t *testing.T) {
	ctx := context.Background()
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "gpu-node-1",
			Labels: map[string]string{TriggerLabel: "0"},
		},
	}
	cs := fake.NewSimpleClientset(node)

	shouldPanic := true
	runner := newTestWatcherRunner(t, nil)
	runner.mode = &fakeMode{
		preResetFn: func(ctx context.Context, gpus []PhysicalGPU) error {
			if shouldPanic {
				panic("simulated panic in preReset")
			}
			return nil
		},
	}
	nw := newRecordingNodeWriter(t, NewNodeWriter(cs, "gpu-node-1"))
	w := NewWatcher(cs, "gpu-node-1", runner, nw)

	w.handle(ctx, "0")

	updatedNode, err := cs.CoreV1().Nodes().Get(ctx, "gpu-node-1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := updatedNode.Labels[TriggerLabel]; exists {
		t.Errorf("TriggerLabel still set after panic: %v", updatedNode.Labels)
	}
	persisted := assertPersistedNodeStatus(t, updatedNode, CodeReconcileFailed)
	if persisted.LastRequest.Phase != PhasePreReset || !strings.Contains(persisted.LastRequest.Message, "simulated panic in preReset") {
		t.Errorf("persisted LastRequest after panic = %+v", persisted.LastRequest)
	}

	shouldPanic = false
	patch := []byte(fmt.Sprintf(`{"metadata":{"labels":{%q:"0"}}}`, TriggerLabel))
	if _, err := cs.CoreV1().Nodes().Patch(ctx, "gpu-node-1", types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		t.Fatal(err)
	}
	w.handle(ctx, "0")

	updatedNode, err = cs.CoreV1().Nodes().Get(ctx, "gpu-node-1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertPersistedNodeStatus(t, updatedNode, CodeSucceeded)
}

func TestWatcher_ForcedAndBothTriggerLabels(t *testing.T) {
	t.Run("forced reset label records CodeUnsupportedMode and clears label", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		node := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-node-1", Labels: map[string]string{"other-key": "keep-me"}}}
		cs := fake.NewSimpleClientset(node)

		var ops []string
		devs := newFakeDevices([]PhysicalGPU{"nvidia0", "nvidia1"}, nil, &ops)
		mon := &fakeMonitors{running: true, log: &ops}
		nv := newFlowNVML(map[PhysicalGPU]string{"nvidia0": "0000:05:00.0", "nvidia1": "0000:06:00.0"}, &ops)
		runner := newTestFlowRunner(t, Config{}, NewMode(Config{}, WithNVML(nv)), devs, mon, nv, nil)

		nw := newRecordingNodeWriter(t, NewNodeWriter(cs, "gpu-node-1"))
		w := NewWatcher(cs, "gpu-node-1", runner, nw)

		doneCh := make(chan error, 1)
		go func() { doneCh <- w.Start(ctx) }()
		waitWatchEstablished(t, cs)

		patch := []byte(fmt.Sprintf(`{"metadata":{"labels":{%q:"0"}}}`, ForcedResetLabel))
		if _, err := cs.CoreV1().Nodes().Patch(ctx, "gpu-node-1", types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
			t.Fatal(err)
		}

		select {
		case final := <-nw.onFinal:
			if final.LastRequest.OverallStatus != CodeUnsupportedMode || final.LastRequest.Kind != KindForced || final.LastRequest.Target != "0" {
				t.Errorf("final LastRequest = %+v", final.LastRequest)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for forced reset final status")
		}

		cancel()
		_ = <-doneCh
		if len(ops) != 0 {
			t.Errorf("expected no hardware/health ops for unsupported forced mode, got %v", ops)
		}
		gotNode, err := cs.CoreV1().Nodes().Get(context.Background(), "gpu-node-1", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		assertPersistedNodeStatus(t, gotNode, CodeUnsupportedMode)
	})

	t.Run("both trigger labels records CodeInvalidTarget and clears both", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		node := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-node-1", Labels: map[string]string{"other-key": "keep-me"}}}
		cs := fake.NewSimpleClientset(node)

		var ops []string
		devs := newFakeDevices([]PhysicalGPU{"nvidia0", "nvidia1"}, nil, &ops)
		runner := newTestFlowRunner(t, Config{}, NewMode(Config{}), devs, nil, nil, nil)

		nw := newRecordingNodeWriter(t, NewNodeWriter(cs, "gpu-node-1"))
		w := NewWatcher(cs, "gpu-node-1", runner, nw)

		doneCh := make(chan error, 1)
		go func() { doneCh <- w.Start(ctx) }()
		waitWatchEstablished(t, cs)

		patch := []byte(fmt.Sprintf(`{"metadata":{"labels":{%q:"0",%q:"1"}}}`, TriggerLabel, ForcedResetLabel))
		if _, err := cs.CoreV1().Nodes().Patch(ctx, "gpu-node-1", types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
			t.Fatal(err)
		}

		select {
		case final := <-nw.onFinal:
			if final.LastRequest.OverallStatus != CodeInvalidTarget {
				t.Errorf("final OverallStatus = %q, want %q", final.LastRequest.OverallStatus, CodeInvalidTarget)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for both-labels final status")
		}

		cancel()
		_ = <-doneCh
		gotNode, err := cs.CoreV1().Nodes().Get(context.Background(), "gpu-node-1", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok1 := gotNode.Labels[TriggerLabel]; ok1 {
			t.Errorf("TriggerLabel not cleared: %v", gotNode.Labels)
		}
		if _, ok2 := gotNode.Labels[ForcedResetLabel]; ok2 {
			t.Errorf("ForcedResetLabel not cleared: %v", gotNode.Labels)
		}
		assertPersistedNodeStatus(t, gotNode, CodeInvalidTarget)
	})
}

func TestKubeNodeWriter_MergePatchAndEvents(t *testing.T) {
	// Verify every defined Code constant is a valid Kubernetes label value (<= 63 chars, never JSON).
	allCodes := []Code{
		CodeInProgress, CodeSucceeded, CodePartialFailure,
		CodeInvalidTarget, CodeUnsupportedNode, CodeUnsupportedMode, CodeRequiresAll,
		CodeNVMLFailed, CodeGPUBusy, CodeHardwareFailure, CodeReconcileFailed, CodeInterrupted,
	}
	for _, c := range allCodes {
		if errs := validation.IsValidLabelValue(string(c)); c == "" || len(errs) > 0 {
			t.Fatalf("Code %q is not a valid K8s label value: %v", c, errs)
		}
	}

	ctx := context.Background()
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "node-a",
			Labels:      map[string]string{TriggerLabel: "0-1", "existing": "label"},
			Annotations: map[string]string{"existing-ann": "val"},
		},
	}
	cs := fake.NewSimpleClientset(node)
	nw := NewNodeWriter(cs, "node-a").(*kubeNodeWriter)
	if nw.broadcaster != nil {
		t.Cleanup(nw.broadcaster.Shutdown)
	}
	fakeRec := record.NewFakeRecorder(5)
	nw.recorder = fakeRec

	// Invalid / JSON / empty / >63-char OverallStatus must be rejected before patching the node.
	cs.ClearActions()
	for _, bad := range []Code{"", `{"overallStatus":"Succeeded"}`, "Error With Spaces", Code(strings.Repeat("a", 64))} {
		if err := nw.WriteStatus(ctx, Status{LastRequest: Request{OverallStatus: bad}}); err == nil {
			t.Fatalf("WriteStatus(%q) succeeded, want error rejecting invalid label value", bad)
		}
	}
	if actions := cs.Actions(); len(actions) != 0 {
		t.Fatalf("expected 0 API actions on invalid status codes, got %+v", actions)
	}

	// Valid WriteStatus must patch BOTH StatusLabel and StatusDetailsAnnotation together in ONE merge patch.
	cs.ClearActions()
	s := Status{LastRequest: Request{Target: "0-1", Phase: PhaseDone, OverallStatus: CodeSucceeded}}
	if err := nw.WriteStatus(ctx, s); err != nil {
		t.Fatalf("WriteStatus: %v", err)
	}
	actions := cs.Actions()
	if len(actions) != 1 {
		t.Fatalf("WriteStatus emitted %d actions (%+v), want exactly 1 node patch", len(actions), actions)
	}
	pa, ok := actions[0].(ktesting.PatchAction)
	if !ok || pa.GetPatchType() != types.MergePatchType {
		t.Fatalf("action[0] = %T (%v), want MergePatchType PatchAction", actions[0], actions[0])
	}
	var patchDoc struct {
		Metadata struct {
			Labels      map[string]string `json:"labels"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(pa.GetPatch(), &patchDoc); err != nil {
		t.Fatalf("unmarshal merge patch: %v", err)
	}
	if patchDoc.Metadata.Labels[StatusLabel] != string(CodeSucceeded) || patchDoc.Metadata.Annotations[StatusDetailsAnnotation] == "" {
		t.Fatalf("single patch did not set both StatusLabel and StatusDetailsAnnotation: %+v", patchDoc)
	}
	if _, hasOld := patchDoc.Metadata.Annotations[StatusLabel]; hasOld {
		t.Fatalf("patch unexpectedly wrote annotation %s: %+v", StatusLabel, patchDoc)
	}

	prev, err := nw.ClearTrigger(ctx)
	if err != nil || prev != "0-1" {
		t.Fatalf("ClearTrigger = (%q, %v), want (0-1, nil)", prev, err)
	}
	nw.Event(v1.EventTypeWarning, ReasonGPUResetRejected, "rejected 2")
	select {
	case ev := <-fakeRec.Events:
		if !strings.Contains(ev, ReasonGPUResetRejected) {
			t.Errorf("unexpected event: %q", ev)
		}
	default:
		t.Error("expected event in recorder")
	}

	got, err := cs.CoreV1().Nodes().Get(ctx, "node-a", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Labels[TriggerLabel]; ok || got.Labels["existing"] != "label" || got.Annotations["existing-ann"] != "val" {
		t.Errorf("unexpected node state: labels=%v annotations=%v", got.Labels, got.Annotations)
	}
	assertPersistedNodeStatus(t, got, CodeSucceeded)
}

func TestWatcher_StartupCrashRecovery(t *testing.T) {
	t.Run("details annotation InProgress restores persistence and converges target GPUs only", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		inProgStatus := Status{
			LastRequest: Request{Target: "0", Phase: PhaseReset, OverallStatus: CodeInProgress},
			Devices:     map[PhysicalGPU]DeviceStatus{"nvidia0": {PCIBusID: "0000:05:00.0", Status: CodeInProgress}},
		}
		rawStatus, _ := json.Marshal(inProgStatus)
		node := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-node-1", Annotations: map[string]string{StatusDetailsAnnotation: string(rawStatus)}}}
		cs := fake.NewSimpleClientset(node)

		var ops []string
		devs := newFakeDevices([]PhysicalGPU{"nvidia0", "nvidia1"}, map[string]Health{"nvidia0": Unhealthy, "nvidia1": Healthy}, &ops)
		mode := &fakeMode{ids: map[PhysicalGPU][]string{"nvidia0": {"nvidia0"}, "nvidia1": {"nvidia1"}}, log: &ops}
		nv := newFlowNVML(map[PhysicalGPU]string{"nvidia0": "0000:05:00.0", "nvidia1": "0000:06:00.0"}, &ops)
		exec := &fakeExecutor{}
		runner := newTestFlowRunner(t, Config{}, mode, devs, nil, nv, exec)

		nw := newRecordingNodeWriter(t, NewNodeWriter(cs, "gpu-node-1"))
		w := NewWatcher(cs, "gpu-node-1", runner, nw)

		doneCh := make(chan error, 1)
		go func() { doneCh <- w.Start(ctx) }()

		select {
		case s := <-nw.onFinal:
			if s.LastRequest.OverallStatus != CodeInterrupted || s.LastRequest.Target != "0" {
				t.Fatalf("LastRequest = %+v, want CodeInterrupted on 0", s.LastRequest)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for interrupted status")
		}

		cancel()
		_ = <-doneCh

		if calls := exec.Calls(); len(calls) != 0 {
			t.Errorf("exec calls = %+v, want 0 (persistence restored via NVML)", calls)
		}
		wantOps := []string{"nvml.SetPersistence(nvidia0,true)", "mode.Converge([nvidia0])", "SetHealth(nvidia0,Healthy)"}
		if !reflect.DeepEqual(ops, wantOps) || devs.Health("nvidia0") != Healthy {
			t.Errorf("ops = %v (want %v), health = %q", ops, wantOps, devs.Health("nvidia0"))
		}
		gotNode, err := cs.CoreV1().Nodes().Get(context.Background(), "gpu-node-1", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		assertPersistedNodeStatus(t, gotNode, CodeInterrupted)
	})

	t.Run("status label InProgress without in-progress details annotation treated as equivalent", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Simulate node carrying cloud.google.com/gpu-reset-status=InProgress while details annotation holds a stale completed run.
		stalePrev, _ := json.Marshal(Status{
			LastRequest: Request{Target: "0", Phase: PhaseDone, OverallStatus: CodeSucceeded},
			Devices:     map[PhysicalGPU]DeviceStatus{"nvidia0": {PCIBusID: "0000:05:00.0", Status: CodeSucceeded}},
		})
		node := &v1.Node{ObjectMeta: metav1.ObjectMeta{
			Name:        "gpu-node-1",
			Labels:      map[string]string{StatusLabel: string(CodeInProgress)},
			Annotations: map[string]string{StatusDetailsAnnotation: string(stalePrev)},
		}}
		cs := fake.NewSimpleClientset(node)

		var ops []string
		devs := newFakeDevices([]PhysicalGPU{"nvidia0", "nvidia1"}, map[string]Health{"nvidia0": Unhealthy, "nvidia1": Unhealthy}, &ops)
		mode := &fakeMode{ids: map[PhysicalGPU][]string{"nvidia0": {"nvidia0"}, "nvidia1": {"nvidia1"}}, log: &ops}
		nv := newFlowNVML(map[PhysicalGPU]string{"nvidia0": "0000:05:00.0", "nvidia1": "0000:06:00.0"}, &ops)
		runner := newTestFlowRunner(t, Config{}, mode, devs, nil, nv, &fakeExecutor{})

		nw := newRecordingNodeWriter(t, NewNodeWriter(cs, "gpu-node-1"))
		w := NewWatcher(cs, "gpu-node-1", runner, nw)

		doneCh := make(chan error, 1)
		go func() { doneCh <- w.Start(ctx) }()

		select {
		case s := <-nw.onFinal:
			if s.LastRequest.OverallStatus != CodeInterrupted {
				t.Fatalf("OverallStatus = %q, want %q", s.LastRequest.OverallStatus, CodeInterrupted)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for interrupted status from StatusLabel=InProgress")
		}

		cancel()
		_ = <-doneCh

		if devs.Health("nvidia0") != Healthy || devs.Health("nvidia1") != Healthy {
			t.Errorf("health after label=InProgress recovery: nvidia0=%q nvidia1=%q, want both Healthy", devs.Health("nvidia0"), devs.Health("nvidia1"))
		}
		gotNode, err := cs.CoreV1().Nodes().Get(context.Background(), "gpu-node-1", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		assertPersistedNodeStatus(t, gotNode, CodeInterrupted)
	})

	t.Run("converge failure leaves GPU unhealthy", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		node := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-node-1", Labels: map[string]string{TriggerLabel: "0"}}}
		cs := fake.NewSimpleClientset(node)

		var ops []string
		devs := newFakeDevices([]PhysicalGPU{"nvidia0", "nvidia1"}, map[string]Health{"nvidia0": Healthy, "nvidia1": Healthy}, &ops)
		mode := &fakeMode{
			ids:        map[PhysicalGPU][]string{"nvidia0": {"nvidia0"}, "nvidia1": {"nvidia1"}},
			convergeFn: func(ctx context.Context, gpus []PhysicalGPU) error { return fmt.Errorf("mig partition create failed") },
			log:        &ops,
		}
		nv := newFlowNVML(map[PhysicalGPU]string{"nvidia0": "0000:05:00.0", "nvidia1": "0000:06:00.0"}, nil)
		runner := newTestFlowRunner(t, Config{}, mode, devs, nil, nv, nil)

		nw := newRecordingNodeWriter(t, NewNodeWriter(cs, "gpu-node-1"))
		w := NewWatcher(cs, "gpu-node-1", runner, nw)

		doneCh := make(chan error, 1)
		go func() { doneCh <- w.Start(ctx) }()

		select {
		case s := <-nw.onFinal:
			if s.LastRequest.OverallStatus != CodeInterrupted {
				t.Fatalf("OverallStatus = %q, want %q", s.LastRequest.OverallStatus, CodeInterrupted)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for interrupted status")
		}

		cancel()
		_ = <-doneCh

		if devs.Health("nvidia0") != Unhealthy || devs.Health("nvidia1") != Healthy {
			t.Errorf("nvidia0=%q nvidia1=%q, want Unhealthy and Healthy", devs.Health("nvidia0"), devs.Health("nvidia1"))
		}
		gotNode, err := cs.CoreV1().Nodes().Get(context.Background(), "gpu-node-1", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		assertPersistedNodeStatus(t, gotNode, CodeInterrupted)
	})
}
