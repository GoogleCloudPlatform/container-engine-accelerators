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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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
	want := `[{"op":"test","path":"/metadata/labels/cloud.google.com~1trigger-gpu-diagnostics","value":""},` +
		`{"op":"remove","path":"/metadata/labels/cloud.google.com~1trigger-gpu-diagnostics"}]`
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
		{name: "empty value", labels: map[string]string{TriggerLabel: ""}, annotations: map[string]string{}, wantRuns: 1, wantPatches: 1, wantStatus: StatusCompleted, wantTrigger: TriggerManual, wantEvent: "Normal GPUDiagnosticsCompleted"},
		{name: "no annotations map", labels: map[string]string{TriggerLabel: "x"}, wantRuns: 1, wantPatches: 1, wantStatus: StatusCompleted, wantTrigger: TriggerManual, wantEvent: "Normal GPUDiagnosticsCompleted"},
		{name: "existing annotations", labels: map[string]string{TriggerLabel: "x"}, annotations: map[string]string{"other": "v"}, wantRuns: 1, wantPatches: 1, wantStatus: StatusCompleted, wantTrigger: TriggerManual, wantEvent: "Normal GPUDiagnosticsCompleted"},
		{
			name:        "manual after failed auto updates status annotation",
			labels:      map[string]string{TriggerLabel: "true"},
			annotations: map[string]string{StatusAnnotation: "failed: timeout"},
			wantRuns:    1,
			wantPatches: 1,
			wantStatus:  StatusCompleted,
			wantTrigger: TriggerManual,
			wantEvent:   "Normal GPUDiagnosticsCompleted",
		},
		{name: "label changed mid-run", labels: map[string]string{TriggerLabel: "x"}, annotations: map[string]string{}, relabel: "y", wantRuns: 1, wantPatches: 3, wantLabel: true, wantStatus: StatusCompleted, wantTrigger: TriggerManual, wantEvent: "Normal GPUDiagnosticsCompleted"},
		{
			name:        "automatic collect",
			annotations: map[string]string{StatusAnnotation: StatusCollect},
			wantRuns:    1,
			wantPatches: 2, // ack (in-progress) + finish (completed + output)
			wantStatus:  StatusCompleted,
			wantTrigger: TriggerAutomatic,
			wantEvent:   "Normal GPUDiagnosticsCompleted",
		},
		{
			name:        "automatic collect failed timeout",
			annotations: map[string]string{StatusAnnotation: StatusCollect},
			runResult:   &Result{Status: StatusFailed, Reason: ReasonTimeout, FilePath: "/partial.log.gz", Size: 10, Start: time.Unix(0, 0), Duration: time.Minute},
			wantRuns:    1,
			wantPatches: 2,
			wantStatus:  "failed: timeout",
			wantTrigger: TriggerAutomatic,
			wantEvent:   "Warning GPUDiagnosticsFailed",
		},
		{
			name:        "automatic disabled by label",
			labels:      map[string]string{DisableLabel: ""},
			annotations: map[string]string{StatusAnnotation: StatusCollect},
			wantRuns:    0,
			wantPatches: 1,
			wantStatus:  "failed: disabled",
			wantEvent:   "Warning GPUDiagnosticsFailed",
		},
		{
			name:        "automatic disable label false",
			labels:      map[string]string{DisableLabel: "false"},
			annotations: map[string]string{StatusAnnotation: StatusCollect},
			wantRuns:    1,
			wantPatches: 2,
			wantStatus:  StatusCompleted,
			wantTrigger: TriggerAutomatic,
			wantEvent:   "Normal GPUDiagnosticsCompleted",
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
			wantEvent:   "Normal GPUDiagnosticsCompleted",
		},
		{
			name:        "coalesced automatic and manual with status changed mid-run removes label and writes output",
			labels:      map[string]string{TriggerLabel: "req1"},
			annotations: map[string]string{StatusAnnotation: StatusCollect},
			restatus:    StatusCollect,
			wantRuns:    1,
			wantPatches: 6, // ack + restatus + combined(fail) + label-remove(ok) + status+output(fail) + output-only(ok)
			wantLabel:   false,
			wantStatus:  StatusCollect,
			wantTrigger: TriggerAutomatic,
			wantEvent:   "Normal GPUDiagnosticsCompleted",
		},
		{
			name:        "coalesced automatic and manual with label changed mid-run updates status and output",
			labels:      map[string]string{TriggerLabel: "req1"},
			annotations: map[string]string{StatusAnnotation: StatusCollect},
			relabel:     "req2",
			wantRuns:    1,
			wantPatches: 5, // ack + relabel + combined(fail) + label-remove(fail) + status+output(ok)
			wantLabel:   true,
			wantStatus:  StatusCompleted,
			wantTrigger: TriggerAutomatic,
			wantEvent:   "Normal GPUDiagnosticsCompleted",
		},
		{
			name:        "startup recovery marks in-progress interrupted",
			annotations: map[string]string{StatusAnnotation: StatusInProgress},
			wantRuns:    0,
			wantPatches: 1,
			wantStatus:  "failed: interrupted",
			wantEvent:   "Warning GPUDiagnosticsFailed",
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
		if !strings.HasPrefix(ev, "Normal GPUDiagnosticsCompleted") {
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

func hasWatch(cs *fake.Clientset) bool {
	for _, a := range cs.Actions() {
		if _, ok := a.(k8stesting.WatchAction); ok {
			return true
		}
	}
	return false
}
