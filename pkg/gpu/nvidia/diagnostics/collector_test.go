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
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeScript(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, "nvidia-bug-report.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func newTestCollector(t *testing.T, script string, timeout time.Duration) (*Collector, string) {
	c, reports, _ := newTestCollectorWithLog(t, script, timeout)
	return c, reports
}

func newTestCollectorWithLog(t *testing.T, script string, timeout time.Duration) (*Collector, string, *bytes.Buffer) {
	t.Helper()
	reports := filepath.Join(t.TempDir(), "reports")
	var logBuf bytes.Buffer
	c := NewCollector(CollectorConfig{
		ReportsDir:       reports,
		ScriptCandidates: []string{"/nonexistent/nvidia-bug-report.sh", script},
		Timeout:          timeout,
		WaitDelay:        time.Second,
		MaxReports:       3,
		Log:              &logBuf,
	})
	return c, reports, &logBuf
}

func TestCollectorCompleted(t *testing.T) {
	script := writeScript(t, t.TempDir(), `
[ "$1" = "--output-file" ] || exit 3
[ "$(pwd)" = "$REPORTS" ] || exit 4
[ "$TMPDIR" = "$REPORTS" ] || exit 5
echo scratch > nvidia-nvml-temp123.log
echo dump > "$TMPDIR/nvidia-debugdump-temp$$.log"
echo noisy-progress-stdout
echo harmless-stderr >&2
{ echo section1 | gzip -c; echo "End of NVIDIA bug report log file." | gzip -c; } > "$2.gz"
exit 1  # the real script's exit code is unreliable; the marker decides
`)
	c, reports, logBuf := newTestCollectorWithLog(t, script, 10*time.Second)
	res := c.Run(context.Background(), TriggerManual)
	if res.Status != StatusCompleted || !res.Complete {
		t.Fatalf("got %+v, want completed", res)
	}
	if logBuf.Len() != 0 {
		t.Errorf("successful run should log nothing, got %q", logBuf.String())
	}
	if !strings.HasPrefix(res.FilePath, reports+"/nvidia-bug-report-") || !strings.HasSuffix(res.FilePath, "-manual.log.gz") {
		t.Errorf("unexpected file path %q", res.FilePath)
	}
	if left, _ := filepath.Glob(filepath.Join(reports, "*-temp*")); len(left) != 0 {
		t.Errorf("temp files not cleaned: %v", left)
	}
	if st, _ := os.Stat(reports); st.Mode().Perm() != 0o700 {
		t.Errorf("reports dir mode %v, want 0700", st.Mode().Perm())
	}
}

func TestCollectorIncomplete(t *testing.T) {
	script := writeScript(t, t.TempDir(), `echo noisy-progress-stdout; echo nvidia-smi-exploded >&2
echo partial | gzip -c > "$2.gz"; exit 2`)
	c, _, logBuf := newTestCollectorWithLog(t, script, 10*time.Second)
	res := c.Run(context.Background(), TriggerManual)
	if res.Status != StatusFailed || res.Reason != "exit-2" || res.Complete || res.FilePath == "" {
		t.Fatalf("got %+v, want failed exit-2 with partial file", res)
	}
	if !strings.Contains(logBuf.String(), "nvidia-smi-exploded") {
		t.Errorf("failed run should log the stderr tail, got %q", logBuf.String())
	}
	if strings.Contains(logBuf.String(), "noisy-progress-stdout") {
		t.Errorf("stdout should never be logged, got %q", logBuf.String())
	}
}

func TestTailBuffer(t *testing.T) {
	tb := &tailBuffer{max: 8}
	tb.Write([]byte("abc"))
	tb.Write([]byte("defgh"))
	if got := tb.String(); got != "abcdefgh" {
		t.Errorf("got %q, want abcdefgh", got)
	}
	tb.Write([]byte("ij"))
	if got := tb.String(); got != "cdefghij" {
		t.Errorf("got %q, want cdefghij", got)
	}
	tb.Write([]byte("0123456789"))
	if got := tb.String(); got != "23456789" {
		t.Errorf("got %q, want 23456789", got)
	}
}

func TestCollectorScriptNotFound(t *testing.T) {
	c, _ := newTestCollector(t, "/also/missing.sh", 10*time.Second)
	res := c.Run(context.Background(), TriggerManual)
	if res.Status != StatusFailed || res.Reason != ReasonScriptNotFound {
		t.Fatalf("got %+v, want script-not-found", res)
	}
}

func TestCollectorTimeoutKillsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	script := writeScript(t, dir, fmt.Sprintf(`echo partial | gzip -c > "$2.gz"
sleep 100 & echo $! > %s
sleep 100`, pidFile))
	c, _ := newTestCollector(t, script, 500*time.Millisecond)
	start := time.Now()
	res := c.Run(context.Background(), TriggerManual)
	if res.Status != StatusFailed || res.Reason != ReasonTimeout {
		t.Fatalf("got %+v, want timeout", res)
	}
	if res.FilePath == "" {
		t.Errorf("partial report should be kept")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("Run took %v, want prompt kill", d)
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	fmt.Sscan(string(b), &pid)
	if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); err == nil {
		st, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if !strings.Contains(string(st), ") Z ") {
			t.Errorf("background child %d survived the process-group kill", pid)
		}
	}
}

func TestCollectorRotation(t *testing.T) {
	script := writeScript(t, t.TempDir(), `echo "End of NVIDIA bug report log file." | gzip -c > "$2.gz"`)
	c, reports := newTestCollector(t, script, 10*time.Second)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		c.now = func() time.Time { return ts }
		if res := c.Run(context.Background(), TriggerManual); res.Status != StatusCompleted {
			t.Fatalf("run %d: %+v", i, res)
		}
	}
	entries, _ := os.ReadDir(reports)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 3 || !strings.Contains(names[0], "20260101T000200Z") {
		t.Fatalf("want the newest 3 reports, got %v", names)
	}
}

func TestReportCompleteTruncatedGzip(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write([]byte("End of NVIDIA bug report log file.\n")); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "r.log.gz")
	if err := os.WriteFile(p, buf.Bytes()[:buf.Len()-4], 0o600); err != nil { // drop part of the trailer
		t.Fatal(err)
	}
	if reportComplete(p) {
		t.Errorf("truncated gzip reported as complete")
	}
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if !reportComplete(p) {
		t.Errorf("complete gzip reported as incomplete")
	}
}

func TestDropPageCache(t *testing.T) {
	p := filepath.Join(t.TempDir(), "report.log.gz")
	if err := os.WriteFile(p, bytes.Repeat([]byte("x"), 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := dropPageCache(p); err != nil {
		t.Errorf("dropPageCache(existing) = %v, want nil", err)
	}
	if err := dropPageCache(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("dropPageCache(missing) = nil, want error")
	}
}
