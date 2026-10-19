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
	"k8s.io/client-go/util/workqueue"
)

// Runner performs one collection (implemented by Collector).
type Runner interface {
	Run(ctx context.Context, trigger Trigger) Result
}

// pendingResult holds a completed collection whose post-run Node patch failed with a transient API
// error, allowing the workqueue retry to publish the result without re-running the script.
type pendingResult struct {
	res               Result
	trigger           Trigger
	wantManual        bool
	seenManual        string
	wantStatus        bool
	hasExpectedStatus bool
	expectedStatus    string
	noAnnotationsMap  bool
}

// Controller watches its own Node and reconciles collection requests using a rate-limiting
// workqueue. It is level-triggered: watch events enqueue the node name, and the single worker
// acts on the latest observed Node. That makes coalescing free: requests that arrive during a run
// are deduplicated by the workqueue and handled once after the current run finishes.
type Controller struct {
	client   kubernetes.Interface
	recorder record.EventRecorder
	runner   Runner
	node     string
	logf     func(format string, args ...any)

	queue          workqueue.RateLimitingInterface
	mu             sync.Mutex
	latest         *v1.Node
	startupChecked bool
	pending        *pendingResult
}

// NewController returns a Controller for the named Node.
func NewController(client kubernetes.Interface, recorder record.EventRecorder, runner Runner, node string) *Controller {
	return &Controller{
		client:   client,
		recorder: recorder,
		runner:   runner,
		node:     node,
		logf:     log.Printf,
		queue:    workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
	}
}

func (c *Controller) observe(n *v1.Node) {
	c.mu.Lock()
	c.latest = n
	c.mu.Unlock()
	c.queue.Add(c.node)
}

func (c *Controller) snapshot() *v1.Node {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.latest
}

// Run blocks until ctx is done.
func (c *Controller) Run(ctx context.Context) {
	go func() {
		<-ctx.Done()
		c.queue.ShutDown()
	}()
	go c.watchLoop(ctx)
	for c.processNextWorkItem(ctx) {
	}
}

func (c *Controller) processNextWorkItem(ctx context.Context) bool {
	item, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(item)

	n := c.snapshot()
	if n == nil {
		c.queue.Forget(item)
		return true
	}
	if err := c.reconcile(ctx, n); err != nil {
		if ctx.Err() == nil {
			c.logf("reconcile %s: %v (re-queueing with backoff)", c.node, err)
			c.queue.AddRateLimited(item)
		}
		return true
	}
	c.queue.Forget(item)
	return true
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

// reconcile handles startup recovery (L2), pending post-collection patch retries, the
// maintenance-handler handshake on StatusAnnotation, and the manual TriggerLabel.
func (c *Controller) reconcile(ctx context.Context, n *v1.Node) error {
	if !c.startupChecked {
		if err := c.recoverStartup(ctx, n); err != nil {
			return err
		}
		c.startupChecked = true
	}

	if c.pending != nil {
		if err := c.finishCollection(ctx, c.pending); err != nil {
			return err
		}
		c.pending = nil
	}

	_, snapshotManual := labelActive(n.Labels, TriggerLabel)
	snapshotAuto := n.Annotations[StatusAnnotation] == StatusCollect
	if !snapshotManual && !snapshotAuto {
		return nil
	}

	// The snapshot may predate our own last patch (events queued during a run), so confirm
	// against a fresh read before starting a multi-second collection.
	n, err := c.client.CoreV1().Nodes().Get(ctx, c.node, metav1.GetOptions{})
	if err != nil {
		c.logf("get node before collection: %v", err)
		return err
	}
	seenManual, wantManual := labelActive(n.Labels, TriggerLabel)
	wantAuto := n.Annotations[StatusAnnotation] == StatusCollect
	if !wantManual && !wantAuto {
		return nil
	}

	if wantAuto {
		if _, disabled := labelActive(n.Labels, DisableLabel); disabled {
			c.logf("automatic trigger observed, but %s is active; reporting disabled", DisableLabel)
			ops := []patchOp{
				{Op: "test", Path: annotationPath(StatusAnnotation), Value: StatusCollect},
				{Op: "replace", Path: annotationPath(StatusAnnotation), Value: StatusFailed + ": " + ReasonDisabled},
			}
			if err := c.patch(ctx, ops); err != nil {
				if !isCASConflict(err) {
					c.logf("patch disabled status: %v", err)
					return err
				}
			} else {
				c.event(Result{Status: StatusFailed, Reason: ReasonDisabled})
			}
			wantAuto = false
			if !wantManual {
				return nil
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
					return err
				}
				wantAuto = false
				if !wantManual {
					return nil
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

	expectedStatus, hasExpectedStatus := n.Annotations[StatusAnnotation]
	if wantAuto {
		expectedStatus = StatusInProgress
		hasExpectedStatus = true
	}

	res := c.runner.Run(ctx, trigger)
	c.logf("collection finished: trigger=%s status=%s reason=%q file=%s size=%d complete=%v duration=%v",
		trigger, res.Status, res.Reason, res.FilePath, res.Size, res.Complete, res.Duration.Round(time.Millisecond))

	p := &pendingResult{
		res:               res,
		trigger:           trigger,
		wantManual:        wantManual,
		seenManual:        seenManual,
		wantStatus:        true,
		hasExpectedStatus: hasExpectedStatus,
		expectedStatus:    expectedStatus,
		noAnnotationsMap:  len(n.Annotations) == 0 && !wantAuto,
	}
	if err := c.finishCollection(ctx, p); err != nil {
		c.pending = p
		return err
	}
	// Collections are rare; hand the heap grown by gzip scanning and API I/O back to the
	// kernel now rather than waiting for the background scavenger. Costs ~1ms.
	debug.FreeOSMemory()
	return nil
}

// finishCollection patches the Node after a collection finishes and emits the completion event.
// In the common case, it performs a single atomic JSON-Patch. If a CAS precondition fails (e.g.
// TriggerLabel or StatusAnnotation was modified mid-run), it applies the TriggerLabel removal,
// StatusAnnotation update, and OutputAnnotation update independently so one conflict cannot block
// the others.
func (c *Controller) finishCollection(ctx context.Context, p *pendingResult) error {
	status := p.res.FormatStatus()
	outBytes, _ := json.Marshal(p.res.Output(p.trigger))
	out := string(outBytes)

	var ops []patchOp
	if p.wantManual {
		// Only remove the label if it still has the value we acted on. If the user re-set it
		// mid-run, the test fails and the next reconcile collects again.
		ops = append(ops,
			patchOp{Op: "test", Path: labelPath(TriggerLabel), Value: p.seenManual},
			patchOp{Op: "remove", Path: labelPath(TriggerLabel)},
		)
	}
	ops = append(ops, finishAnnotationOps(p.noAnnotationsMap, p.wantStatus, p.hasExpectedStatus, p.expectedStatus, status, out)...)

	if err := c.patch(ctx, ops); err != nil {
		if !isCASConflict(err) {
			c.logf("patch node: %v", err)
			return err
		}
		// At least one CAS precondition failed. If both TriggerLabel and StatusAnnotation had
		// preconditions, try removing TriggerLabel on its own so a StatusAnnotation conflict
		// does not leave TriggerLabel behind.
		if p.wantManual && p.wantStatus && p.hasExpectedStatus {
			labelOps := []patchOp{
				{Op: "test", Path: labelPath(TriggerLabel), Value: p.seenManual},
				{Op: "remove", Path: labelPath(TriggerLabel)},
			}
			if err := c.patch(ctx, labelOps); err != nil && !isCASConflict(err) {
				c.logf("patch node trigger label: %v", err)
				return err
			}
		}
		p.wantManual = false

		// Now try updating StatusAnnotation (with CAS check if applicable) and OutputAnnotation.
		if err := c.patch(ctx, finishAnnotationOps(p.noAnnotationsMap, p.wantStatus, p.hasExpectedStatus, p.expectedStatus, status, out)); err != nil {
			if !isCASConflict(err) {
				c.logf("patch node status and output: %v", err)
				return err
			}
			// StatusAnnotation changed mid-run (e.g. a new "collect" request arrived): leave
			// StatusAnnotation untouched on this and any subsequent workqueue retries, and
			// publish OutputAnnotation alone.
			p.wantStatus = false
			if err := c.patch(ctx, outputOnlyOps(p.noAnnotationsMap, out)); err != nil && !isCASConflict(err) {
				c.logf("patch node output: %v", err)
				return err
			}
		}
	}

	c.event(p.res)
	return nil
}

// recoverStartup implements L2 deadlock recovery: if a previous container crashed or OOMed while
// StatusAnnotation was "in-progress", transition it to "failed: interrupted" on startup.
func (c *Controller) recoverStartup(ctx context.Context, n *v1.Node) error {
	if n.Annotations[StatusAnnotation] != StatusInProgress {
		return nil
	}
	fresh, err := c.client.CoreV1().Nodes().Get(ctx, c.node, metav1.GetOptions{})
	if err != nil {
		c.logf("get node for startup recovery: %v", err)
		return err
	}
	if fresh.Annotations[StatusAnnotation] != StatusInProgress {
		return nil
	}
	c.logf("found stale %s=%q at startup; marking interrupted", StatusAnnotation, StatusInProgress)
	ops := []patchOp{
		{Op: "test", Path: annotationPath(StatusAnnotation), Value: StatusInProgress},
		{Op: "replace", Path: annotationPath(StatusAnnotation), Value: StatusFailed + ": " + ReasonInterrupted},
	}
	if err := c.patch(ctx, ops); err != nil && !isCASConflict(err) {
		c.logf("patch interrupted status: %v", err)
		return err
	}
	c.event(Result{Status: StatusFailed, Reason: ReasonInterrupted})
	return nil
}

func isCASConflict(err error) bool {
	if apierrors.IsInvalid(err) || apierrors.IsConflict(err) {
		return true
	}
	// client-go's fake clientset returns raw jsonpatch test errors without wrapping in StatusError.
	return err != nil && strings.Contains(err.Error(), "testing value ")
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

// finishAnnotationOps builds the JSON-Patch operations to publish StatusAnnotation (when
// wantStatus is true) and OutputAnnotation after a collection finishes. Updating StatusAnnotation
// on both manual and automatic runs prevents StatusAnnotation and OutputAnnotation from drifting
// apart.
func finishAnnotationOps(noAnnotationsMap, wantStatus, hasExpectedStatus bool, expectedStatus, status, output string) []patchOp {
	if !wantStatus {
		return outputOnlyOps(noAnnotationsMap, output)
	}
	if hasExpectedStatus {
		return []patchOp{
			{Op: "test", Path: annotationPath(StatusAnnotation), Value: expectedStatus},
			{Op: "replace", Path: annotationPath(StatusAnnotation), Value: status},
			{Op: "add", Path: annotationPath(OutputAnnotation), Value: output},
		}
	}
	if noAnnotationsMap {
		return []patchOp{{
			Op:   "add",
			Path: "/metadata/annotations",
			Value: map[string]string{
				StatusAnnotation: status,
				OutputAnnotation: output,
			},
		}}
	}
	return []patchOp{
		{Op: "add", Path: annotationPath(StatusAnnotation), Value: status},
		{Op: "add", Path: annotationPath(OutputAnnotation), Value: output},
	}
}

func outputOnlyOps(noAnnotationsMap bool, output string) []patchOp {
	if noAnnotationsMap {
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
