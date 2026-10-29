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
	"sync/atomic"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
)

// slowSink simulates API server latency for event writes.
type slowSink struct {
	delay   time.Duration
	created atomic.Int32
}

func (s *slowSink) Create(e *v1.Event) (*v1.Event, error) {
	time.Sleep(s.delay)
	s.created.Add(1)
	return e, nil
}
func (s *slowSink) Update(e *v1.Event) (*v1.Event, error)          { return e, nil }
func (s *slowSink) Patch(e *v1.Event, _ []byte) (*v1.Event, error) { return e, nil }

// TestShutdownEventsFlushesPendingEvent verifies that an event recorded right before exit (as
// the final GPUReportFailed "interrupted" event is on SIGTERM) is written before shutdownEvents
// returns, even when the API server is slow.
func TestShutdownEventsFlushesPendingEvent(t *testing.T) {
	slow := &slowSink{delay: 300 * time.Millisecond}
	sink := &trackingSink{EventSink: slow}
	b := record.NewBroadcaster()
	b.StartRecordingToSink(sink)
	rec := b.NewRecorder(scheme.Scheme, v1.EventSource{Component: "gpu-bug-report", Host: "n1"})

	rec.Eventf(&v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}, v1.EventTypeWarning, "GPUReportFailed", "interrupted")
	start := time.Now()
	shutdownEvents(b, sink, 100*time.Millisecond, 5*time.Second)

	if got := slow.created.Load(); got != 1 {
		t.Fatalf("events written before exit = %d, want 1", got)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("shutdownEvents took %v, want well under the 5s timeout", d)
	}
}

// TestShutdownEventsBoundedByTimeout verifies a hung API server cannot block exit indefinitely.
func TestShutdownEventsBoundedByTimeout(t *testing.T) {
	slow := &slowSink{delay: 2 * time.Second}
	sink := &trackingSink{EventSink: slow}
	b := record.NewBroadcaster()
	b.StartRecordingToSink(sink)
	rec := b.NewRecorder(scheme.Scheme, v1.EventSource{Component: "gpu-bug-report", Host: "n1"})

	rec.Eventf(&v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}, v1.EventTypeWarning, "GPUReportFailed", "interrupted")
	start := time.Now()
	shutdownEvents(b, sink, 100*time.Millisecond, 500*time.Millisecond)
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Errorf("shutdownEvents took %v, want it bounded by the 500ms timeout", d)
	}
}
