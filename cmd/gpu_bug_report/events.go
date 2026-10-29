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
	"log"
	"sync/atomic"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
)

// trackingSink records when event writes are in flight so the process can wait for its last
// events (e.g. GPUReportFailed "interrupted" on SIGTERM) before exiting. The broadcaster hands
// events to the sink on a background goroutine, and Shutdown only waits until they are queued
// for that goroutine, not until they reach the API server.
type trackingSink struct {
	record.EventSink
	inflight   atomic.Int32
	lastActive atomic.Int64 // UnixNano of the last write start/end
}

func (s *trackingSink) begin() { s.inflight.Add(1); s.touch() }
func (s *trackingSink) end()   { s.touch(); s.inflight.Add(-1) }
func (s *trackingSink) touch() { s.lastActive.Store(time.Now().UnixNano()) }

func (s *trackingSink) Create(e *v1.Event) (*v1.Event, error) {
	s.begin()
	defer s.end()
	return s.EventSink.Create(e)
}

func (s *trackingSink) Update(e *v1.Event) (*v1.Event, error) {
	s.begin()
	defer s.end()
	return s.EventSink.Update(e)
}

func (s *trackingSink) Patch(e *v1.Event, data []byte) (*v1.Event, error) {
	s.begin()
	defer s.end()
	return s.EventSink.Patch(e, data)
}

// waitIdle returns true once no write is in flight and none has started or finished for quiet,
// or false if timeout elapses first.
func (s *trackingSink) waitIdle(quiet, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if s.inflight.Load() == 0 && time.Since(time.Unix(0, s.lastActive.Load())) >= quiet {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// shutdownEvents stops the broadcaster and waits (bounded by timeout) for queued events to be
// written. Shutdown first hands every queued event to the sink goroutine; the quiet period gives
// that goroutine time to pick them up before we decide it is idle.
func shutdownEvents(b record.EventBroadcaster, s *trackingSink, quiet, timeout time.Duration) {
	b.Shutdown()
	s.touch()
	if !s.waitIdle(quiet, timeout) {
		log.Printf("timed out after %v waiting for pending events to be written", timeout)
	}
}
