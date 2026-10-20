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

// Package diagnostics implements the GPU bug-report executor: a small, self-contained
// component that watches its own Node, runs the host's nvidia-bug-report.sh on request,
// and publishes the result on the Node.
//
// It runs in its own always-on container on every GPU node, so keep dependencies to
// client-go and the standard library. Do not import NVML or gRPC here: the executor must
// stay cgo-free and must not touch the GPUs itself (nvidia-bug-report.sh does that).
package diagnostics

import "time"

const (
	// TriggerLabel is a presence-based manual trigger. It is active unless its value is "false".
	// The executor removes it after the collection finishes.
	TriggerLabel = "cloud.google.com/trigger-gpu-report"
	// DisableLabel disables automatic collections requested by the maintenance handler.
	DisableLabel = "cloud.google.com/disable-gpu-report"
	// StatusAnnotation carries the automatic handshake with the maintenance handler.
	StatusAnnotation = "cloud.google.com/gpu-report-status"
	// OutputAnnotation holds the JSON-encoded Output of the most recent collection.
	OutputAnnotation = "cloud.google.com/gpu-report-output"
)

// Collection status values (used in StatusAnnotation and Output.Status).
const (
	StatusCollect    = "collect"
	StatusInProgress = "in-progress"
	StatusCompleted  = "completed"
	StatusFailed     = "failed"
)

// Failure reasons.
const (
	ReasonDisabled       = "disabled"
	ReasonInterrupted    = "interrupted"
	ReasonTimeout        = "timeout"
	ReasonScriptNotFound = "script-not-found"
	ReasonBusy           = "busy"
	ReasonInternal       = "internal"
)

// FormatStatus formats a Result for StatusAnnotation ("completed" or "failed: <reason>").
func (r Result) FormatStatus() string {
	if r.Status == StatusCompleted {
		return StatusCompleted
	}
	if r.Reason == "" {
		return StatusFailed
	}
	return StatusFailed + ": " + r.Reason
}

// Trigger identifies what requested a collection.
type Trigger string

const (
	TriggerManual    Trigger = "manual"
	TriggerAutomatic Trigger = "automatic"
)

// Output is published as JSON in OutputAnnotation.
type Output struct {
	Status          string  `json:"status"`
	Reason          string  `json:"reason,omitempty"`
	Trigger         Trigger `json:"trigger"`
	FilePath        string  `json:"filePath,omitempty"`
	FileSizeBytes   int64   `json:"fileSizeBytes,omitempty"`
	Complete        bool    `json:"complete"`
	DurationSeconds float64 `json:"durationSeconds"`
	CollectedAt     string  `json:"collectedAt"`
}

// Result is what a single collection produced.
type Result struct {
	Status   string
	Reason   string
	FilePath string // path on the host
	Size     int64
	Complete bool
	Start    time.Time
	Duration time.Duration
}

// Output converts a Result into the annotation payload.
func (r Result) Output(t Trigger) Output {
	return Output{
		Status:          r.Status,
		Reason:          r.Reason,
		Trigger:         t,
		FilePath:        r.FilePath,
		FileSizeBytes:   r.Size,
		Complete:        r.Complete,
		DurationSeconds: float64(r.Duration.Milliseconds()) / 1000,
		CollectedAt:     r.Start.UTC().Format(time.RFC3339),
	}
}

// labelActive reports whether a presence-based label is set and not explicitly "false".
func labelActive(labels map[string]string, key string) (string, bool) {
	v, ok := labels[key]
	return v, ok && v != "false"
}
