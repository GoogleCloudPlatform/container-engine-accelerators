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
	"log"
	"math/rand"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
)

// Runner performs one collection (implemented by Collector).
type Runner interface {
	Run(ctx context.Context, trigger Trigger) Result
}

// Controller watches its own Node and reconciles collection requests. It is level-triggered:
// watch events only "kick" a single worker, which acts on the latest observed Node. That makes
// coalescing free: requests that arrive during a run are handled once, after it.
type Controller struct {
	client   kubernetes.Interface
	recorder record.EventRecorder
	runner   Runner
	node     string
	logf     func(format string, args ...any)

	kick           chan struct{}
	mu             sync.Mutex
	latest         *v1.Node
	startupChecked bool
}

// NewController returns a Controller for the named Node.
func NewController(client kubernetes.Interface, recorder record.EventRecorder, runner Runner, node string) *Controller {
	return &Controller{
		client:   client,
		recorder: recorder,
		runner:   runner,
		node:     node,
		logf:     log.Printf,
		kick:     make(chan struct{}, 1),
	}
}

func (c *Controller) observe(n *v1.Node) {
	c.mu.Lock()
	c.latest = n
	c.mu.Unlock()
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

func (c *Controller) snapshot() *v1.Node {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.latest
}

// Run blocks until ctx is done.
func (c *Controller) Run(ctx context.Context) {
	go c.watchLoop(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.kick:
			if n := c.snapshot(); n != nil {
				c.reconcile(ctx, n)
			}
		}
	}
}

// watchLoop gets then watches the Node, re-getting on 410 or errors and re-watching on close.
// (A single-object informer would do the same, but tools/cache is not vendored here.)
func (c *Controller) watchLoop(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		n, err := c.client.CoreV1().Nodes().Get(ctx, c.node, metav1.GetOptions{})
		if err != nil {
			c.logf("get node %s: %v (retry in %v)", c.node, err, backoff)
			sleepCtx(ctx, backoff)
			backoff = min(2*backoff, time.Minute)
			continue
		}
		backoff = time.Second
		c.observe(n)
		rv := n.ResourceVersion
		for ctx.Err() == nil {
			if rv, err = c.watchOnce(ctx, rv); err != nil {
				if !apierrors.IsResourceExpired(err) && !apierrors.IsGone(err) && ctx.Err() == nil {
					c.logf("watch node %s: %v", c.node, err)
					sleepCtx(ctx, time.Second)
				}
				break // re-get
			}
		}
	}
}

// watchOnce watches the Node from rv until the server ends the stream and returns the last
// resourceVersion seen.
func (c *Controller) watchOnce(ctx context.Context, rv string) (string, error) {
	// Randomize the server-side timeout (5–10m) so nodes don't reconnect in lockstep.
	timeoutSec := int64(300 + rand.Intn(300))
	w, err := c.client.CoreV1().Nodes().Watch(ctx, metav1.ListOptions{
		FieldSelector:       fields.OneTermEqualSelector("metadata.name", c.node).String(),
		ResourceVersion:     rv,
		AllowWatchBookmarks: true,
		TimeoutSeconds:      &timeoutSec,
	})
	if err != nil {
		return rv, err
	}
	defer w.Stop()
	for ev := range w.ResultChan() {
		if ev.Type == watch.Error {
			return rv, apierrors.FromObject(ev.Object)
		}
		n, ok := ev.Object.(*v1.Node)
		if !ok {
			continue
		}
		rv = n.ResourceVersion
		if ev.Type == watch.Added || ev.Type == watch.Modified {
			c.observe(n)
		}
	}
	return rv, ctx.Err()
}

// reconcile handles startup recovery (L2), the maintenance-handler handshake on StatusAnnotation,
// and the manual TriggerLabel.
func (c *Controller) reconcile(ctx context.Context, n *v1.Node) {
	if !c.startupChecked {
		if !c.recoverStartup(ctx, n) {
			return
		}
		c.startupChecked = true
	}

	_, snapshotManual := labelActive(n.Labels, TriggerLabel)
	snapshotAuto := n.Annotations[StatusAnnotation] == StatusCollect
	if !snapshotManual && !snapshotAuto {
		return
	}

	// The snapshot may predate our own last patch (events queued during a run), so confirm
	// against a fresh read before starting a multi-second collection.
	n, err := c.client.CoreV1().Nodes().Get(ctx, c.node, metav1.GetOptions{})
	if err != nil {
		c.logf("get node before collection: %v", err)
		return
	}
	seenManual, wantManual := labelActive(n.Labels, TriggerLabel)
	wantAuto := n.Annotations[StatusAnnotation] == StatusCollect
	if !wantManual && !wantAuto {
		return
	}

	if wantAuto {
		if _, disabled := labelActive(n.Labels, DisableLabel); disabled {
			c.logf("automatic trigger observed, but %s is active; reporting disabled", DisableLabel)
			ops := []patchOp{
				{Op: "test", Path: annotationPath(StatusAnnotation), Value: StatusCollect},
				{Op: "replace", Path: annotationPath(StatusAnnotation), Value: StatusFailed + ": " + ReasonDisabled},
			}
			if err := c.patch(ctx, ops); err != nil && !isCASConflict(err) {
				c.logf("patch disabled status: %v", err)
			} else if err == nil {
				c.event(Result{Status: StatusFailed, Reason: ReasonDisabled})
			}
			wantAuto = false
			if !wantManual {
				return
			}
		} else {
			// Acknowledge the request by transitioning collect -> in-progress.
			ackOps := []patchOp{
				{Op: "test", Path: annotationPath(StatusAnnotation), Value: StatusCollect},
				{Op: "replace", Path: annotationPath(StatusAnnotation), Value: StatusInProgress},
			}
			if err := c.patch(ctx, ackOps); err != nil {
				if !isCASConflict(err) {
					c.logf("patch in-progress status: %v", err)
				}
				wantAuto = false
				if !wantManual {
					return
				}
			}
		}
	}

	trigger := TriggerManual
	if wantAuto {
		trigger = TriggerAutomatic
		c.logf("automatic trigger acknowledged (coalesced manual=%v), starting collection", wantManual)
	} else {
		c.logf("manual trigger observed (label %s=%q), starting collection", TriggerLabel, seenManual)
	}

	res := c.runner.Run(ctx, trigger)
	c.logf("collection finished: trigger=%s status=%s reason=%q file=%s size=%d complete=%v duration=%v",
		trigger, res.Status, res.Reason, res.FilePath, res.Size, res.Complete, res.Duration.Round(time.Millisecond))

	outBytes, _ := json.Marshal(res.Output(trigger))
	out := string(outBytes)

	var ops []patchOp
	if wantManual {
		// Only remove the label if it still has the value we acted on. If the user re-set it
		// mid-run, the test fails and the next reconcile collects again.
		ops = append(ops,
			patchOp{Op: "test", Path: labelPath(TriggerLabel), Value: seenManual},
			patchOp{Op: "remove", Path: labelPath(TriggerLabel)},
		)
	}
	ops = append(ops, finishAnnotationOps(n, wantAuto, res.FormatStatus(), out)...)

	if err := c.patch(ctx, ops); err != nil {
		if !isCASConflict(err) {
			c.logf("patch node: %v", err)
		}
		// Label changed or removed mid-run: still publish the status/output, leave the label alone.
		if err := c.patch(ctx, finishAnnotationOps(n, wantAuto, res.FormatStatus(), out)); err != nil && !isCASConflict(err) {
			c.logf("patch node output: %v", err)
		}
	}
	c.event(res)
	// Collections are rare; hand the heap grown by gzip scanning and API I/O back to the
	// kernel now rather than waiting for the background scavenger. Costs ~1ms.
	debug.FreeOSMemory()
}

// recoverStartup implements L2 deadlock recovery: if a previous container crashed or OOMed while
// StatusAnnotation was "in-progress", transition it to "failed: interrupted" on startup.
func (c *Controller) recoverStartup(ctx context.Context, n *v1.Node) bool {
	if n.Annotations[StatusAnnotation] != StatusInProgress {
		return true
	}
	fresh, err := c.client.CoreV1().Nodes().Get(ctx, c.node, metav1.GetOptions{})
	if err != nil {
		c.logf("get node for startup recovery: %v", err)
		return false
	}
	if fresh.Annotations[StatusAnnotation] != StatusInProgress {
		return true
	}
	c.logf("found stale %s=%q at startup; marking interrupted", StatusAnnotation, StatusInProgress)
	ops := []patchOp{
		{Op: "test", Path: annotationPath(StatusAnnotation), Value: StatusInProgress},
		{Op: "replace", Path: annotationPath(StatusAnnotation), Value: StatusFailed + ": " + ReasonInterrupted},
	}
	if err := c.patch(ctx, ops); err != nil && !isCASConflict(err) {
		c.logf("patch interrupted status: %v", err)
		return false
	}
	c.event(Result{Status: StatusFailed, Reason: ReasonInterrupted})
	return true
}

func isCASConflict(err error) bool {
	return apierrors.IsInvalid(err) || apierrors.IsConflict(err)
}

// patchOp is one RFC 6902 JSON-Patch operation.
type patchOp struct {
	Op    string
	Path  string
	Value any
}

// MarshalJSON omits "value" only for "remove" operations so that empty-string values (common on
// presence-based labels) are always serialized for "test", "add", and "replace".
func (p patchOp) MarshalJSON() ([]byte, error) {
	if p.Op == "remove" {
		return json.Marshal(struct {
			Op   string `json:"op"`
			Path string `json:"path"`
		}{Op: p.Op, Path: p.Path})
	}
	return json.Marshal(struct {
		Op    string `json:"op"`
		Path  string `json:"path"`
		Value any    `json:"value"`
	}{Op: p.Op, Path: p.Path, Value: p.Value})
}

var pointerEscaper = strings.NewReplacer("~", "~0", "/", "~1")

func labelPath(key string) string      { return "/metadata/labels/" + pointerEscaper.Replace(key) }
func annotationPath(key string) string { return "/metadata/annotations/" + pointerEscaper.Replace(key) }

func (c *Controller) patch(ctx context.Context, ops []patchOp) error {
	body, err := json.Marshal(ops)
	if err != nil {
		return err
	}
	_, err = c.client.CoreV1().Nodes().Patch(ctx, c.node, types.JSONPatchType, body, metav1.PatchOptions{})
	return err
}

// finishAnnotationOps builds the JSON-Patch operations to publish OutputAnnotation and (for
// automatic runs) transition StatusAnnotation from "in-progress" to its terminal value.
func finishAnnotationOps(n *v1.Node, wantAuto bool, status, output string) []patchOp {
	if wantAuto {
		return []patchOp{
			{Op: "test", Path: annotationPath(StatusAnnotation), Value: StatusInProgress},
			{Op: "replace", Path: annotationPath(StatusAnnotation), Value: status},
			{Op: "add", Path: annotationPath(OutputAnnotation), Value: output},
		}
	}
	if len(n.Annotations) == 0 {
		return []patchOp{{Op: "add", Path: "/metadata/annotations", Value: map[string]string{OutputAnnotation: output}}}
	}
	return []patchOp{{Op: "add", Path: annotationPath(OutputAnnotation), Value: output}}
}

func (c *Controller) event(res Result) {
	// kubelet uses the node name as the UID for Node events; `kubectl describe node` matches on it.
	ref := &v1.ObjectReference{Kind: "Node", Name: c.node, UID: types.UID(c.node)}
	if res.Status == StatusCompleted {
		c.recorder.Eventf(ref, v1.EventTypeNormal, "GPUDiagnosticsCompleted",
			"NVIDIA bug report written to %s (%d bytes) in %.1fs", res.FilePath, res.Size, res.Duration.Seconds())
		return
	}
	c.recorder.Eventf(ref, v1.EventTypeWarning, "GPUDiagnosticsFailed",
		"NVIDIA bug report failed: %s (file %q, %.1fs)", res.Reason, res.FilePath, res.Duration.Seconds())
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
