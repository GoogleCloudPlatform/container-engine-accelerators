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

package diagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
)

func TestPatchOpJSON(t *testing.T) {
	b, err := json.Marshal([]patchOp{
		{Op: "test", Path: labelPath(TriggerLabel), Value: ""},
		{Op: "remove", Path: labelPath(TriggerLabel)},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"op":"test","path":"/metadata/labels/cloud.google.com~1trigger-gpu-report","value":""},` +
		`{"op":"remove","path":"/metadata/labels/cloud.google.com~1trigger-gpu-report"}]`
	if string(b) != want {
		t.Errorf("got %s\nwant %s", b, want)
	}
}

// fakeRunner optionally runs a hook mid-collection (e.g. to simulate a user relabeling).
type fakeRunner struct {
	calls  int
	during func()
	result *Result
}

func (r *fakeRunner) Run(ctx context.Context, t Trigger) Result {
	r.calls++
	if r.during != nil {
		r.during()
	}
	if r.result != nil {
		return *r.result
	}
	return Result{
		Status:   StatusCompleted,
		FilePath: "/r.log.gz",
		Size:     42,
		Complete: true,
		Start:    time.Unix(0, 0),
		Duration: time.Second,
	}
}

func patchCount(cs *fake.Clientset) int {
	n := 0
	for _, a := range cs.Actions() {
		if a.GetVerb() == "patch" {
			n++
		}
	}
	return n
}

func TestReconcile(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name        string
		labels      map[string]string
		annotations map[string]string
		relabel     string // if set, the trigger label is changed to this value mid-run
		restatus    string // if set, the status annotation is changed to this value mid-run
		runResult   *Result
		wantRuns    int
		wantPatches int
		wantLabel   bool // trigger label still present afterwards
		wantStatus  string
		wantTrigger Trigger
		wantEvent   string
	}{
		{name: "no label", labels: map[string]string{}},
		{name: "label false", labels: map[string]string{TriggerLabel: "false"}, wantLabel: true},
		{name: "empty value", labels: map[string]string{TriggerLabel: ""}, annotations: map[string]string{}, wantRuns: 1, wantPatches: 1, wantStatus: StatusCompleted, wantTrigger: TriggerManual, wantEvent: "Normal GPUReportCompleted"},
		{name: "no annotations map", labels: map[string]string{TriggerLabel: "x"}, wantRuns: 1, wantPatches: 1, wantStatus: StatusCompleted, wantTrigger: TriggerManual, wantEvent: "Normal GPUReportCompleted"},
		{name: "existing annotations", labels: map[string]string{TriggerLabel: "x"}, annotations: map[string]string{"other": "v"}, wantRuns: 1, wantPatches: 1, wantStatus: StatusCompleted, wantTrigger: TriggerManual, wantEvent: "Normal GPUReportCompleted"},
		{
			name:        "manual after failed auto updates status annotation",
			labels:      map[string]string{TriggerLabel: "true"},
			annotations: map[string]string{StatusAnnotation: "failed: timeout"},
			wantRuns:    1,
			wantPatches: 1,
			wantStatus:  StatusCompleted,
			wantTrigger: TriggerManual,
			wantEvent:   "Normal GPUReportCompleted",
		},
		{name: "label changed mid-run", labels: map[string]string{TriggerLabel: "x"}, annotations: map[string]string{}, relabel: "y", wantRuns: 1, wantPatches: 3, wantLabel: true, wantStatus: StatusCompleted, wantTrigger: TriggerManual, wantEvent: "Normal GPUReportCompleted"},
		{
			name:        "automatic collect",
			annotations: map[string]string{StatusAnnotation: StatusCollect},
			wantRuns:    1,
			wantPatches: 2, // ack (in-progress) + finish (completed + output)
			wantStatus:  StatusCompleted,
			wantTrigger: TriggerAutomatic,
			wantEvent:   "Normal GPUReportCompleted",
		},
		{
			name:        "automatic collect failed timeout",
			annotations: map[string]string{StatusAnnotation: StatusCollect},
			runResult:   &Result{Status: StatusFailed, Reason: ReasonTimeout, FilePath: "/partial.log.gz", Size: 10, Start: time.Unix(0, 0), Duration: time.Minute},
			wantRuns:    1,
			wantPatches: 2,
			wantStatus:  "failed: timeout",
			wantTrigger: TriggerAutomatic,
			wantEvent:   "Warning GPUReportFailed",
		},
		{
			name:        "automatic disabled by label",
			labels:      map[string]string{DisableLabel: ""},
			annotations: map[string]string{StatusAnnotation: StatusCollect},
			wantRuns:    0,
			wantPatches: 1,
			wantStatus:  "failed: disabled",
			wantEvent:   "Warning GPUReportFailed",
		},
		{
			name:        "automatic disable label false",
			labels:      map[string]string{DisableLabel: "false"},
			annotations: map[string]string{StatusAnnotation: StatusCollect},
			wantRuns:    1,
			wantPatches: 2,
			wantStatus:  StatusCompleted,
			wantTrigger: TriggerAutomatic,
			wantEvent:   "Normal GPUReportCompleted",
		},
		{
			name:        "coalesced automatic and manual",
			labels:      map[string]string{TriggerLabel: "req1"},
			annotations: map[string]string{StatusAnnotation: StatusCollect},
			wantRuns:    1,
			wantPatches: 2,
			wantLabel:   false,
			wantStatus:  StatusCompleted,
			wantTrigger: TriggerAutomatic,
			wantEvent:   "Normal GPUReportCompleted",
		},
		{
			name:        "coalesced automatic and manual with status changed mid-run removes label and writes output",
			labels:      map[string]string{TriggerLabel: "req1"},
			annotations: map[string]string{StatusAnnotation: StatusCollect},
			restatus:    StatusCollect,
			wantRuns:    1,
			wantPatches: 4, // ack + restatus + combined(fail) + narrowed label-remove+output(ok)
			wantLabel:   false,
			wantStatus:  StatusCollect,
			wantTrigger: TriggerAutomatic,
			wantEvent:   "Normal GPUReportCompleted",
		},
		{
			name:        "coalesced automatic and manual with label changed mid-run updates status and output",
			labels:      map[string]string{TriggerLabel: "req1"},
			annotations: map[string]string{StatusAnnotation: StatusCollect},
			relabel:     "req2",
			wantRuns:    1,
			wantPatches: 4, // ack + relabel + combined(fail) + narrowed status+output(ok)
			wantLabel:   true,
			wantStatus:  StatusCompleted,
			wantTrigger: TriggerAutomatic,
			wantEvent:   "Normal GPUReportCompleted",
		},
		{
			name:        "startup recovery marks in-progress interrupted",
			annotations: map[string]string{StatusAnnotation: StatusInProgress},
			wantRuns:    0,
			wantPatches: 1,
			wantStatus:  "failed: interrupted",
			wantEvent:   "Warning GPUReportFailed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "n1",
					Labels:      tc.labels,
					Annotations: tc.annotations,
				},
			}
			cs := fake.NewSimpleClientset(node)
			rec := record.NewFakeRecorder(10)
			r := &fakeRunner{result: tc.runResult}
			if tc.relabel != "" || tc.restatus != "" {
				r.during = func() {
					if tc.relabel != "" {
						p := `[{"op":"replace","path":"` + labelPath(TriggerLabel) + `","value":"` + tc.relabel + `"}]`
						if _, err := cs.CoreV1().Nodes().Patch(ctx, "n1", types.JSONPatchType, []byte(p), metav1.PatchOptions{}); err != nil {
							t.Fatal(err)
						}
					}
					if tc.restatus != "" {
						p := `[{"op":"replace","path":"` + annotationPath(StatusAnnotation) + `","value":"` + tc.restatus + `"}]`
						if _, err := cs.CoreV1().Nodes().Patch(ctx, "n1", types.JSONPatchType, []byte(p), metav1.PatchOptions{}); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			c := NewController(cs, rec, r, "n1")
			c.logf = t.Logf
			n, _ := cs.CoreV1().Nodes().Get(ctx, "n1", metav1.GetOptions{})
			if err := c.reconcile(ctx, n); err != nil {
				t.Fatalf("reconcile: %v", err)
			}

			if r.calls != tc.wantRuns || patchCount(cs) != tc.wantPatches {
				t.Fatalf("runs=%d patches=%d, want %d/%d", r.calls, patchCount(cs), tc.wantRuns, tc.wantPatches)
			}
			got, _ := cs.CoreV1().Nodes().Get(ctx, "n1", metav1.GetOptions{})
			if _, ok := got.Labels[TriggerLabel]; ok != tc.wantLabel {
				t.Errorf("trigger label present=%v, want %v", ok, tc.wantLabel)
			}
			if tc.wantStatus != "" && got.Annotations[StatusAnnotation] != tc.wantStatus {
				t.Errorf("status annotation = %q, want %q", got.Annotations[StatusAnnotation], tc.wantStatus)
			}
			if tc.wantEvent != "" {
				select {
				case ev := <-rec.Events:
					if !strings.HasPrefix(ev, tc.wantEvent) {
						t.Errorf("event %q, want prefix %q", ev, tc.wantEvent)
					}
				default:
					t.Error("no event recorded")
				}
			}
			if tc.wantRuns == 0 {
				return
			}
			var o Output
			if err := json.Unmarshal([]byte(got.Annotations[OutputAnnotation]), &o); err != nil {
				t.Fatalf("unmarshal output annotation %q: %v", got.Annotations[OutputAnnotation], err)
			}
			if o.Trigger != tc.wantTrigger {
				t.Errorf("output trigger = %q, want %q", o.Trigger, tc.wantTrigger)
			}
		})
	}
}

// TestWatchLoop checks that label changes delivered over the watch trigger a collection.
func TestWatchLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cs := fake.NewSimpleClientset(&v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}})
	done := make(chan struct{})
	r := &fakeRunner{during: func() { close(done) }}
	c := NewController(cs, record.NewFakeRecorder(10), r, "n1")
	c.logf = t.Logf
	go c.Run(ctx)

	// Wait for the watch to be established before modifying the node.
	deadline := time.Now().Add(5 * time.Second)
	for !hasWatch(cs) {
		if time.Now().After(deadline) {
			t.Fatal("watch never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	p := `[{"op":"add","path":"/metadata/labels","value":{"` + TriggerLabel + `":""}}]`
	if _, err := cs.CoreV1().Nodes().Patch(ctx, "n1", types.JSONPatchType, []byte(p), metav1.PatchOptions{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("collection not triggered by watch event")
	}
}

// TestWorkqueueRetryOnTransientErrors verifies that transient Get/Patch errors are re-queued by
// the workqueue and retried without re-running the collection when the finish patch fails once.
func TestWorkqueueRetryOnTransientErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "n1",
			Labels:      map[string]string{TriggerLabel: "true"},
			Annotations: map[string]string{StatusAnnotation: StatusCollect},
		},
	}
	cs := fake.NewSimpleClientset(node)
	var patchCalls atomic.Int32
	cs.PrependReactor("patch", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
		n := patchCalls.Add(1)
		// Fail the 1st ack patch and the 1st post-collection finish patch with transient errors.
		if n == 1 || n == 3 {
			return true, nil, errors.New("transient apiserver error")
		}
		return false, nil, nil
	})

	rec := record.NewFakeRecorder(10)
	r := &fakeRunner{}
	c := NewController(cs, rec, r, "n1")
	c.logf = t.Logf
	go c.Run(ctx)

	select {
	case ev := <-rec.Events:
		if !strings.HasPrefix(ev, "Normal GPUReportCompleted") {
			t.Fatalf("unexpected event: %s", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for workqueue retry to complete collection")
	}

	if r.calls != 1 {
		t.Errorf("runner calls = %d, want 1 (post-collection patch retry must not re-run script)", r.calls)
	}
	got, err := cs.CoreV1().Nodes().Get(ctx, "n1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Annotations[StatusAnnotation] != StatusCompleted {
		t.Errorf("status annotation = %q, want %q", got.Annotations[StatusAnnotation], StatusCompleted)
	}
	if _, ok := got.Labels[TriggerLabel]; ok {
		t.Errorf("trigger label still present after completion")
	}
}

// TestPendingRetryPreservesChangedStatusAnnotation verifies that if StatusAnnotation changes
// mid-run (causing a CAS conflict) AND the narrowed outputOnlyOps patch fails with a transient
// error, the subsequent workqueue retry only patches OutputAnnotation and does not overwrite the
// externally modified StatusAnnotation.
func TestPendingRetryPreservesChangedStatusAnnotation(t *testing.T) {
	ctx := context.Background()
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "n1",
			Annotations: map[string]string{StatusAnnotation: StatusCollect},
		},
	}
	cs := fake.NewSimpleClientset(node)
	var patchCalls int
	cs.PrependReactor("patch", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
		patchCalls++
		// Patch 1: ack (collect -> in-progress)
		// Patch 2: mid-run restatus hook (in-progress -> failed: abandoned)
		// Patch 3: attempt 0 combined finishAnnotationOps (fails CAS on StatusAnnotation)
		// Patch 4: attempt 1 narrowed outputOnlyOps (inject transient error!)
		if patchCalls == 4 {
			return true, nil, errors.New("transient apiserver error on output-only patch")
		}
		return false, nil, nil
	})

	rec := record.NewFakeRecorder(10)
	r := &fakeRunner{
		during: func() {
			p := `[{"op":"replace","path":"` + annotationPath(StatusAnnotation) + `","value":"failed: abandoned"}]`
			if _, err := cs.CoreV1().Nodes().Patch(ctx, "n1", types.JSONPatchType, []byte(p), metav1.PatchOptions{}); err != nil {
				t.Fatal(err)
			}
		},
	}
	c := NewController(cs, rec, r, "n1")
	c.logf = t.Logf
	n, _ := cs.CoreV1().Nodes().Get(ctx, "n1", metav1.GetOptions{})

	// First reconcile fails on the outputOnlyOps transient error and caches c.pending.
	if err := c.reconcile(ctx, n); err == nil {
		t.Fatal("expected first reconcile to return transient error from outputOnlyOps")
	}
	// Second reconcile (simulating workqueue retry) must publish OutputAnnotation without
	// overwriting StatusAnnotation="failed: abandoned".
	n, _ = cs.CoreV1().Nodes().Get(ctx, "n1", metav1.GetOptions{})
	if err := c.reconcile(ctx, n); err != nil {
		t.Fatalf("second reconcile failed: %v", err)
	}
	if r.calls != 1 {
		t.Errorf("runner calls = %d, want 1", r.calls)
	}
	got, err := cs.CoreV1().Nodes().Get(ctx, "n1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Annotations[StatusAnnotation] != "failed: abandoned" {
		t.Errorf("status annotation = %q, want %q (must not be overwritten on retry)", got.Annotations[StatusAnnotation], "failed: abandoned")
	}
	if got.Annotations[OutputAnnotation] == "" {
		t.Errorf("output annotation was not published on retry")
	}
}

// TestNonConflictInvalidErrorNotSwallowed verifies that a 422 Invalid error when the guarded test
// values on the Node have NOT changed is returned as an error rather than treated as a CAS conflict.
func TestNonConflictInvalidErrorNotSwallowed(t *testing.T) {
	ctx := context.Background()
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "n1",
			Annotations: map[string]string{StatusAnnotation: StatusCollect},
		},
	}
	cs := fake.NewSimpleClientset(node)
	cs.PrependReactor("patch", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInvalid(schema.GroupKind{Kind: "Node"}, "n1", nil)
	})
	c := NewController(cs, record.NewFakeRecorder(10), &fakeRunner{}, "n1")
	c.logf = t.Logf
	n, _ := cs.CoreV1().Nodes().Get(ctx, "n1", metav1.GetOptions{})
	if err := c.reconcile(ctx, n); err == nil {
		t.Fatal("expected non-conflict Invalid error to be returned, got nil")
	}
}

// TestSequentialFinishConflictsStillPublishOutput verifies that if TriggerLabel conflicts on
// attempt 0 and StatusAnnotation subsequently conflicts on attempt 1, finishCollection continues
// to attempt 2 and publishes OutputAnnotation via outputOnlyOps rather than returning nil without
// patching the Node.
func TestSequentialFinishConflictsStillPublishOutput(t *testing.T) {
	ctx := context.Background()
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "n1",
			Labels:      map[string]string{TriggerLabel: "v1"},
			Annotations: map[string]string{StatusAnnotation: StatusCollect},
		},
	}
	cs := fake.NewSimpleClientset(node)
	var getCalls int
	cs.PrependReactor("get", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getCalls++
		// Get 1: reconcile pre-collection Get
		// Get 2: finishCollection attempt 0 conflict check (after TriggerLabel changed to "v2").
		// Right after Get 2 returns (so attempt 0 only saw TriggerLabel conflict), mutate
		// StatusAnnotation before attempt 1's Patch executes!
		return false, nil, nil
	})
	var patchCalls int
	cs.PrependReactor("patch", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
		patchCalls++
		// Patch 1: ack (collect -> in-progress)
		// Patch 2: mid-run relabel hook (TriggerLabel: v1 -> v2)
		// Patch 3: finishCollection attempt 0 (fails test on TriggerLabel)
		// Patch 4: finishCollection attempt 1 -> mutate StatusAnnotation right before it runs so
		//          attempt 1 fails test on StatusAnnotation!
		if patchCalls == 4 {
			n, err := cs.Tracker().Get(v1.SchemeGroupVersion.WithResource("nodes"), "", "n1")
			if err != nil {
				t.Fatal(err)
			}
			nodeObj := n.(*v1.Node).DeepCopy()
			nodeObj.Annotations[StatusAnnotation] = "failed: abandoned"
			if err := cs.Tracker().Update(v1.SchemeGroupVersion.WithResource("nodes"), nodeObj, ""); err != nil {
				t.Fatal(err)
			}
		}
		return false, nil, nil
	})

	rec := record.NewFakeRecorder(10)
	r := &fakeRunner{
		during: func() {
			p := `[{"op":"replace","path":"` + labelPath(TriggerLabel) + `","value":"v2"}]`
			if _, err := cs.CoreV1().Nodes().Patch(ctx, "n1", types.JSONPatchType, []byte(p), metav1.PatchOptions{}); err != nil {
				t.Fatal(err)
			}
		},
	}
	c := NewController(cs, rec, r, "n1")
	c.logf = t.Logf
	n, _ := cs.CoreV1().Nodes().Get(ctx, "n1", metav1.GetOptions{})
	if err := c.reconcile(ctx, n); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
	got, err := cs.CoreV1().Nodes().Get(ctx, "n1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Labels[TriggerLabel] != "v2" {
		t.Errorf("trigger label = %q, want %q", got.Labels[TriggerLabel], "v2")
	}
	if got.Annotations[StatusAnnotation] != "failed: abandoned" {
		t.Errorf("status annotation = %q, want %q", got.Annotations[StatusAnnotation], "failed: abandoned")
	}
	if got.Annotations[OutputAnnotation] == "" {
		t.Errorf("output annotation was not published after sequential conflicts")
	}
}

// TestMidRunAnnotationAddedWhenInitiallyEmpty verifies that if a manual run starts on a Node with
// no annotations (noAnnotationsMap=true) and annotations are added mid-run, finishCollection does
// not overwrite the newly created /metadata/annotations map or clobber StatusAnnotation.
func TestMidRunAnnotationAddedWhenInitiallyEmpty(t *testing.T) {
	ctx := context.Background()
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "n1",
			Labels: map[string]string{TriggerLabel: "true"},
		},
	}
	cs := fake.NewSimpleClientset(node)
	rec := record.NewFakeRecorder(10)
	r := &fakeRunner{
		during: func() {
			p := `[{"op":"add","path":"/metadata/annotations","value":{"unrelated":"keep-me","` + StatusAnnotation + `":"` + StatusCollect + `"}}]`
			if _, err := cs.CoreV1().Nodes().Patch(ctx, "n1", types.JSONPatchType, []byte(p), metav1.PatchOptions{}); err != nil {
				t.Fatal(err)
			}
		},
	}
	c := NewController(cs, rec, r, "n1")
	c.logf = t.Logf
	n, _ := cs.CoreV1().Nodes().Get(ctx, "n1", metav1.GetOptions{})
	if err := c.reconcile(ctx, n); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
	got, err := cs.CoreV1().Nodes().Get(ctx, "n1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Annotations["unrelated"] != "keep-me" {
		t.Errorf("unrelated annotation = %q, want %q", got.Annotations["unrelated"], "keep-me")
	}
	if got.Annotations[StatusAnnotation] != StatusCollect {
		t.Errorf("status annotation = %q, want %q (must not clobber mid-run collect)", got.Annotations[StatusAnnotation], StatusCollect)
	}
	if got.Annotations[OutputAnnotation] == "" {
		t.Errorf("output annotation was not published")
	}
}

// TestCoalescedDisabledAutoAndManualUpdatesStatus verifies that when DisableLabel is active while
// both StatusAnnotation=collect and TriggerLabel are present, the auto request is rejected as
// "failed: disabled" and the manual run subsequently updates StatusAnnotation to "completed".
func TestCoalescedDisabledAutoAndManualUpdatesStatus(t *testing.T) {
	ctx := context.Background()
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "n1",
			Labels:      map[string]string{TriggerLabel: "true", DisableLabel: "true"},
			Annotations: map[string]string{StatusAnnotation: StatusCollect},
		},
	}
	cs := fake.NewSimpleClientset(node)
	rec := record.NewFakeRecorder(10)
	r := &fakeRunner{}
	c := NewController(cs, rec, r, "n1")
	c.logf = t.Logf
	n, _ := cs.CoreV1().Nodes().Get(ctx, "n1", metav1.GetOptions{})
	if err := c.reconcile(ctx, n); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
	got, err := cs.CoreV1().Nodes().Get(ctx, "n1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Annotations[StatusAnnotation] != StatusCompleted {
		t.Errorf("status annotation = %q, want %q", got.Annotations[StatusAnnotation], StatusCompleted)
	}
	if _, ok := got.Labels[TriggerLabel]; ok {
		t.Errorf("trigger label still present after manual completion")
	}
	if got.Annotations[OutputAnnotation] == "" {
		t.Errorf("output annotation was not published")
	}
}

func hasWatch(cs *fake.Clientset) bool {
	for _, a := range cs.Actions() {
		if _, ok := a.(k8stesting.WatchAction); ok {
			return true
		}
	}
	return false
}
