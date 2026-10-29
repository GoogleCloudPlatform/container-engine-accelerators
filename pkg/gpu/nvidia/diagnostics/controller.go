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

	queue   workqueue.RateLimitingInterface
	mu      sync.Mutex
	latest  *v1.Node
	pending *pendingResult
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
			prevRV := rv
			if rv, err = c.watchOnce(ctx, rv); err != nil {
				if !apierrors.IsResourceExpired(err) && !apierrors.IsGone(err) && ctx.Err() == nil {
					c.logf("watch node %s: %v", c.node, err)
					sleepCtx(ctx, time.Second)
				}
				break // re-get
			}
			if rv == prevRV && ctx.Err() == nil {
				sleepCtx(ctx, time.Second)
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

// reconcile handles pending post-collection patch retries, orphaned in-progress recovery, the
// maintenance-handler handshake on StatusAnnotation, and the manual TriggerLabel.
func (c *Controller) reconcile(ctx context.Context, n *v1.Node) error {
	if c.pending != nil {
		if err := c.finishCollection(ctx, c.pending); err != nil {
			return err
		}
		c.pending = nil
	}

	// Because reconcile runs on a single synchronous worker and c.pending is nil here, no
	// collection is active in this process. Any "in-progress" status on the Node is orphaned
	// (from a prior container crash or a lost ack response) and must be marked interrupted.
	n, fresh, err := c.recoverStartup(ctx, n, false)
	if err != nil {
		return err
	}

	_, snapshotManual := labelActive(n.Labels, TriggerLabel)
	snapshotAuto := n.Annotations[StatusAnnotation] == StatusCollect
	if !snapshotManual && !snapshotAuto {
		return nil
	}

	// The snapshot may predate our own last patch (events queued during a run), so confirm
	// against a fresh read before starting a multi-second collection.
	if !fresh {
		n, err = c.client.CoreV1().Nodes().Get(ctx, c.node, metav1.GetOptions{})
		if err != nil {
			c.logf("get node before collection: %v", err)
			return err
		}
		n, fresh, err = c.recoverStartup(ctx, n, true)
		if err != nil {
			return err
		}
		if !fresh {
			c.queue.Add(c.node)
			return nil
		}
	}
	seenManual, wantManual := labelActive(n.Labels, TriggerLabel)
	wantAuto := n.Annotations[StatusAnnotation] == StatusCollect
	if !wantManual && !wantAuto {
		return nil
	}

	if wantAuto {
		if _, disabled := labelActive(n.Labels, DisableLabel); disabled {
			c.logf("automatic trigger observed, but %s is active; reporting disabled", DisableLabel)
			updated, err := c.casStatusAnnotation(ctx, StatusCollect, StatusFailed+": "+ReasonDisabled)
			if err != nil {
				c.logf("patch disabled status: %v", err)
				return err
			}
			if !updated {
				if wantManual {
					c.queue.Add(c.node)
				}
				return nil
			}
			c.event(Result{Status: StatusFailed, Reason: ReasonDisabled})
			if n.Annotations == nil {
				n.Annotations = map[string]string{}
			}
			n.Annotations[StatusAnnotation] = StatusFailed + ": " + ReasonDisabled
			wantAuto = false
			if !wantManual {
				return nil
			}
		} else {
			// Acknowledge the request by transitioning collect -> in-progress.
			acked, err := c.casStatusAnnotation(ctx, StatusCollect, StatusInProgress)
			if err != nil {
				c.logf("patch in-progress status: %v", err)
				return err
			}
			if !acked {
				if wantManual {
					c.queue.Add(c.node)
				}
				return nil
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
		res:     res,
		trigger: trigger,
		// Keep TriggerLabel on the Node if the run was aborted by SIGTERM so the replacement pod
		// fulfills the manual request.
		wantManual:        wantManual && res.Reason != ReasonInterrupted,
		seenManual:        seenManual,
		wantStatus:        true,
		hasExpectedStatus: hasExpectedStatus,
		expectedStatus:    expectedStatus,
		noAnnotationsMap:  len(n.Annotations) == 0 && !wantAuto,
	}
	// If ctx was canceled by SIGTERM during collection, use a short cleanup context so we still
	// publish the terminal status ("failed: interrupted") before exiting.
	finishCtx := ctx
	if finishCtx.Err() != nil {
		var cancel context.CancelFunc
		finishCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
	}
	if err := c.finishCollection(finishCtx, p); err != nil {
		c.pending = p
		return err
	}
	// Collections are rare; hand the heap grown by gzip scanning and API I/O back to the
	// kernel now rather than waiting for the background scavenger. Costs ~1ms.
	debug.FreeOSMemory()
	return nil
}

// finishCollection patches the Node after a collection finishes and emits the completion event.
// In the common case, it performs a single atomic JSON-Patch guarded by RFC 6902 "test" ops. If
// the patch fails a precondition check, it fetches the current Node and compares the live
// TriggerLabel and StatusAnnotation values against the expected test values so only the non-
// conflicting fields are updated on the retry.
func (c *Controller) finishCollection(ctx context.Context, p *pendingResult) error {
	status := p.res.FormatStatus()
	outBytes, _ := json.Marshal(p.res.Output(p.trigger))
	out := string(outBytes)

	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
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

		err := c.patch(ctx, ops)
		if err == nil {
			c.event(p.res)
			return nil
		}
		if !isTestFailed(err) {
			c.logf("patch node: %v", err)
			return err
		}
		lastErr = err

		// A JSON-Patch "test" op may have failed: fetch the latest Node and compare its current
		// label and annotation values against our expected test values to identify which field
		// changed mid-run.
		fresh, getErr := c.client.CoreV1().Nodes().Get(ctx, c.node, metav1.GetOptions{})
		if getErr != nil {
			c.logf("get node after patch conflict: %v", getErr)
			return getErr
		}
		conflict := false
		if p.wantManual {
			if cur, ok := fresh.Labels[TriggerLabel]; !ok || cur != p.seenManual {
				p.wantManual = false
				conflict = true
			}
		}
		if p.wantStatus {
			if cur, ok := fresh.Annotations[StatusAnnotation]; ok != p.hasExpectedStatus || cur != p.expectedStatus {
				p.wantStatus = false
				conflict = true
			}
		}
		if noMap := len(fresh.Annotations) == 0; noMap != p.noAnnotationsMap {
			p.noAnnotationsMap = noMap
			conflict = true
		}
		if !conflict {
			c.logf("patch node: %v", err)
			return err
		}
	}

	c.logf("patch node exhausted conflict retries: %v", lastErr)
	return lastErr
}

// recoverStartup handles deadlock recovery: if a previous container crashed/OOMed or a network blip
// dropped an ack response while StatusAnnotation was "in-progress", transition it to
// "failed: interrupted" so the maintenance handler does not wait for a dead collection.
// When isFresh is false, n is a cached watch snapshot and recoverStartup fetches the latest Node
// before patching; when isFresh is true, n was just fetched by the caller. Returns the latest Node
// (with StatusAnnotation updated in-memory if transitioned) and whether n reflects a fresh read.
func (c *Controller) recoverStartup(ctx context.Context, n *v1.Node, isFresh bool) (*v1.Node, bool, error) {
	if n.Annotations[StatusAnnotation] != StatusInProgress {
		return n, isFresh, nil
	}
	if !isFresh {
		fresh, err := c.client.CoreV1().Nodes().Get(ctx, c.node, metav1.GetOptions{})
		if err != nil {
			c.logf("get node for startup recovery: %v", err)
			return nil, false, err
		}
		n = fresh
		isFresh = true
		if n.Annotations[StatusAnnotation] != StatusInProgress {
			return n, true, nil
		}
	}
	c.logf("found stale %s=%q; marking interrupted", StatusAnnotation, StatusInProgress)
	updated, err := c.casStatusAnnotation(ctx, StatusInProgress, StatusFailed+": "+ReasonInterrupted)
	if err != nil {
		c.logf("patch interrupted status: %v", err)
		return nil, false, err
	}
	if updated {
		c.event(Result{Status: StatusFailed, Reason: ReasonInterrupted})
		if n.Annotations == nil {
			n.Annotations = map[string]string{}
		}
		n.Annotations[StatusAnnotation] = StatusFailed + ": " + ReasonInterrupted
		return n, isFresh, nil
	}
	return n, false, nil
}

// casStatusAnnotation atomically transitions StatusAnnotation from expected to next using an
// RFC 6902 JSON-Patch "test" operation. If the patch fails, it fetches the Node and compares the
// live StatusAnnotation value against expected to distinguish a genuine CAS conflict from other
// errors.
func (c *Controller) casStatusAnnotation(ctx context.Context, expected, next string) (bool, error) {
	ops := []patchOp{
		{Op: "test", Path: annotationPath(StatusAnnotation), Value: expected},
		{Op: "replace", Path: annotationPath(StatusAnnotation), Value: next},
	}
	err := c.patch(ctx, ops)
	if err == nil {
		return true, nil
	}
	if !isTestFailed(err) {
		return false, err
	}
	fresh, getErr := c.client.CoreV1().Nodes().Get(ctx, c.node, metav1.GetOptions{})
	if getErr != nil {
		return false, getErr
	}
	if fresh.Annotations[StatusAnnotation] != expected {
		return false, nil
	}
	return false, err
}

// isTestFailed reports whether err is a candidate RFC 6902 "test" precondition failure. Because
// kube-apiserver maps jsonpatch.Apply errors to a generic 422 Invalid StatusError, callers follow
// up with a Get and compare the live field value against the expected test value.
func isTestFailed(err error) bool {
	if apierrors.IsInvalid(err) || apierrors.IsConflict(err) {
		return true
	}
	if err == nil {
		return false
	}
	// client-go's fake clientset returns raw jsonpatch errors without wrapping in StatusError.
	msg := err.Error()
	return strings.Contains(msg, "testing value ") || strings.Contains(msg, "operation does not apply")
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
		return []patchOp{
			{Op: "test", Path: "/metadata/annotations", Value: nil},
			{
				Op:   "add",
				Path: "/metadata/annotations",
				Value: map[string]string{
					StatusAnnotation: status,
					OutputAnnotation: output,
				},
			},
		}
	}
	return []patchOp{
		{Op: "test", Path: annotationPath(StatusAnnotation), Value: nil},
		{Op: "add", Path: annotationPath(StatusAnnotation), Value: status},
		{Op: "add", Path: annotationPath(OutputAnnotation), Value: output},
	}
}

func outputOnlyOps(noAnnotationsMap bool, output string) []patchOp {
	if noAnnotationsMap {
		return []patchOp{
			{Op: "test", Path: "/metadata/annotations", Value: nil},
			{Op: "add", Path: "/metadata/annotations", Value: map[string]string{OutputAnnotation: output}},
		}
	}
	return []patchOp{{Op: "add", Path: annotationPath(OutputAnnotation), Value: output}}
}

func (c *Controller) event(res Result) {
	// kubelet uses the node name as the UID for Node events; `kubectl describe node` matches on it.
	ref := &v1.ObjectReference{Kind: "Node", Name: c.node, UID: types.UID(c.node)}
	if res.Status == StatusCompleted {
		c.recorder.Eventf(ref, v1.EventTypeNormal, "GPUReportCompleted",
			"NVIDIA bug report written to %s (%d bytes) in %.1fs", res.FilePath, res.Size, res.Duration.Seconds())
		return
	}
	c.recorder.Eventf(ref, v1.EventTypeWarning, "GPUReportFailed",
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
