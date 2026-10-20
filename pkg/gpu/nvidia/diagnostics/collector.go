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
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	reportPrefix   = "nvidia-bug-report-"
	completeMarker = "End of NVIDIA bug report log file"
	// exitScriptNotFound is returned by the launcher when no candidate script is readable.
	exitScriptNotFound = 127
	// stderrTailBytes bounds how much script stderr is kept in memory for failure logs.
	stderrTailBytes = 4096
)

// launcher runs inside the host root (after chroot). It picks the first readable candidate,
// derives PATH/LD_LIBRARY_PATH from the script's own driver directory, and execs the script
// from the staging directory ($TMPDIR). Both the working directory and TMPDIR point to a
// dedicated subdirectory under ReportsDir: the script's largest temp file is the
// nvidia-debugdump dump (~1MiB per GPU), and /tmp is tmpfs on COS, whose pages count against
// this container as unreclaimable shmem. Running inside a dedicated staging directory lets us
// move only the finished report into ReportsDir and remove the staging directory afterwards.
// When $JOURNAL_SINCE is set, a small journalctl wrapper is placed in $TMPDIR at the front of
// PATH so nvidia-bug-report.sh's unbounded `journalctl -b` scans are capped to recent logs.
const launcher = `for p in "$@"; do
  if [ -r "$p" ]; then
    d="${p%/*}"
    export PATH="$d:/usr/sbin:/usr/bin:/sbin:/bin" LD_LIBRARY_PATH="${d%/*}/lib64"
    cd "$TMPDIR" || exit 126
    if [ -n "$JOURNAL_SINCE" ]; then
      j=$(command -v journalctl 2>/dev/null)
      if [ -n "$j" ] && [ -x "$j" ]; then
        printf '#!/bin/sh\nexec "%s" --since="%s" "$@"\n' "$j" "$JOURNAL_SINCE" > "$TMPDIR/journalctl"
        chmod 755 "$TMPDIR/journalctl"
        export PATH="$TMPDIR:$PATH"
      fi
    fi
    sh=/bin/bash
    [ -x "$sh" ] || sh=/bin/sh
    exec "$sh" "$p" --output-file "$OUT"
  fi
done
exit 127`

// CollectorConfig configures a Collector.
type CollectorConfig struct {
	// HostRoot is where the host's "/" is mounted in the container (e.g. "/root").
	// Empty disables chroot, which is only useful in tests.
	HostRoot string
	// ReportsDir is the host path that receives reports (e.g. /home/kubernetes/bin/nvidia-bug-reports).
	ReportsDir string
	// ScriptCandidates are host paths tried in order.
	ScriptCandidates []string
	// Timeout bounds one collection. WaitDelay bounds the wait for I/O after the kill.
	Timeout   time.Duration
	WaitDelay time.Duration
	// MaxReports is how many reports to keep.
	MaxReports int
	// MaxLogAge bounds how far back journalctl scans (e.g. 7*24*time.Hour). Zero disables the cap.
	MaxLogAge time.Duration
	// Log receives the tail of the script's stderr when a collection does not complete.
	Log io.Writer
}

// Collector runs nvidia-bug-report.sh. It runs at most one collection at a time.
type Collector struct {
	cfg  CollectorConfig
	mu   sync.Mutex
	busy bool // a previous process group survived SIGKILL (D-state); refuse new runs
	now  func() time.Time
}

// NewCollector returns a Collector.
func NewCollector(cfg CollectorConfig) *Collector {
	if cfg.Log == nil {
		cfg.Log = os.Stderr
	}
	if cfg.WaitDelay == 0 {
		cfg.WaitDelay = 10 * time.Second
	}
	return &Collector{cfg: cfg, now: time.Now}
}

// containerPath maps a host path to the path visible in this container.
func (c *Collector) containerPath(hostPath string) string {
	if c.cfg.HostRoot == "" {
		return hostPath
	}
	return filepath.Join(c.cfg.HostRoot, hostPath)
}

// Run performs one collection and always returns a Result (never panics on script failure).
//
// Performance note: In standard GKE production, nvidia-persistenced is running, keeping the driver
// context warm. Benchmarked collection times with persistence mode active:
//   - 1x RTX PRO 6000 (G4): ~6.7s
//   - 8x RTX PRO 6000 (G4): ~6.4–8.0s
//   - 8x B200 Blackwell (A4): ~13.3–13.6s
//   - 4x GB300 Grace Blackwell Ultra (A4X Max baremetal, arm64): ~17.6–18.8s
//
// If nvidia-persistenced is disabled or crashed on multi-GPU NVLink nodes (A4/A3), collection can
// take ~95–100s because each sequential nvidia-smi / nvidia-debugdump command (~10 separate queries)
// cold-loads the GSP firmware and retrains NVLink on every invocation (~10s per command).
func (c *Collector) Run(ctx context.Context, trigger Trigger) Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	start := c.now()
	res := Result{Status: StatusFailed, Start: start}
	if c.busy {
		res.Reason = ReasonBusy
		return res
	}

	dir := c.containerPath(c.cfg.ReportsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		res.Reason = fmt.Sprintf("%s: mkdir: %v", ReasonInternal, err)
		return res
	}
	_ = os.Chmod(dir, 0o700)

	workHost := filepath.Join(c.cfg.ReportsDir, ".work")
	workDir := c.containerPath(workHost)
	_ = os.RemoveAll(workDir)
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		res.Reason = fmt.Sprintf("%s: mkdir work: %v", ReasonInternal, err)
		return res
	}
	defer os.RemoveAll(workDir)

	base := fmt.Sprintf("%s%s-%s.log", reportPrefix, start.UTC().Format("20060102T150405Z"), trigger)
	stageOutHost := filepath.Join(workHost, base)
	outHost := filepath.Join(c.cfg.ReportsDir, base)

	runCtx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	args := append([]string{"-c", launcher, "sh"}, c.cfg.ScriptCandidates...)
	cmd := exec.CommandContext(runCtx, "/bin/sh", args...)
	cmd.Dir = "/"
	env := []string{
		"PATH=/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/root",
		"LC_ALL=C",
		"OUT=" + stageOutHost,
		"TMPDIR=" + workHost,
	}
	if c.cfg.MaxLogAge > 0 {
		since := start.Add(-c.cfg.MaxLogAge).UTC().Format("2006-01-02 15:04:05 UTC")
		env = append(env, "JOURNAL_SINCE="+since)
	}
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if c.cfg.HostRoot != "" {
		cmd.SysProcAttr.Chroot = c.cfg.HostRoot
	}
	// stdout is only progress noise (~100 lines per run) and goes to /dev/null. stderr is kept
	// as a bounded tail and only logged if the collection does not complete.
	stderr := &tailBuffer{max: stderrTailBytes}
	cmd.Stdout = nil
	cmd.Stderr = stderr
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = c.cfg.WaitDelay
	defer func() {
		if res.Status != StatusCompleted {
			if tail := stderr.String(); tail != "" {
				fmt.Fprintf(c.cfg.Log, "nvidia-bug-report.sh stderr (last %d bytes):\n%s\n", stderrTailBytes, tail)
			}
		}
	}()

	if err := cmd.Start(); err != nil {
		res.Reason = fmt.Sprintf("%s: start: %v", ReasonInternal, err)
		res.Duration = c.now().Sub(start)
		return res
	}
	pgid := cmd.Process.Pid
	runErr := cmd.Wait()
	res.Duration = c.now().Sub(start)
	timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)

	// The script may have left children behind (e.g. a hung nvidia-smi); kill the whole group.
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	if groupAlive(pgid, 5*time.Second) {
		c.busy = true
	}

	exitCode := 0
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		exitCode = ee.ExitCode()
	}
	if exitCode == exitScriptNotFound && !timedOut {
		res.Reason = ReasonScriptNotFound
		return res
	}

	for _, ext := range []string{".gz", ""} {
		src := c.containerPath(stageOutHost + ext)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		name := outHost + ext
		path := c.containerPath(name)
		if err := os.Rename(src, path); err != nil {
			fmt.Fprintf(c.cfg.Log, "move report %s -> %s: %v\n", src, path, err)
			break
		}
		if st, err := os.Stat(path); err == nil {
			res.FilePath = name
			res.Size = st.Size()
			res.Complete = reportComplete(path)
			// The report's page cache is charged to this container's memcg; evict it so
			// multi-MiB reports don't push the executor toward its memory limit.
			if err := dropPageCache(path); err != nil {
				fmt.Fprintf(c.cfg.Log, "drop page cache for %s: %v\n", name, err)
			}
		}
		break
	}
	_ = os.RemoveAll(workDir)
	c.rotate(dir)

	switch {
	case timedOut:
		res.Reason = ReasonTimeout
	case res.Complete:
		// The script's exit code is unreliable (sections fail on missing tools); trust the marker.
		res.Status = StatusCompleted
	case res.FilePath == "":
		res.Reason = fmt.Sprintf("exit-%d", exitCode)
		if runErr != nil && ee == nil {
			res.Reason = fmt.Sprintf("%s: %v", ReasonInternal, runErr)
		}
	default:
		res.Reason = fmt.Sprintf("exit-%d", exitCode)
	}
	if c.busy && res.Status != StatusCompleted {
		res.Reason = ReasonBusy
	}
	return res
}

// groupAlive polls until the process group is gone or the timeout elapses.
func groupAlive(pgid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
			return false
		}
		if time.Now().After(deadline) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// tailBuffer is an io.Writer that keeps only the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(p)
	if n >= t.max {
		t.buf = append(t.buf[:0], p[n-t.max:]...)
		return n, nil
	}
	if over := len(t.buf) + n - t.max; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	t.buf = append(t.buf, p...)
	return n, nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}

// reportComplete stream-decompresses (or reads) the report and looks for the end marker in the
// last few KiB. Reports are ~0.5 MiB (1 GPU) to ~10 MiB (8 GPUs) compressed; the read is
// streaming, so memory use is constant and it takes tens of milliseconds.
func reportComplete(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var r io.Reader = bufio.NewReader(f)
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(r)
		if err != nil {
			return false
		}
		defer gz.Close()
		// The script appends one gzip member per section.
		gz.Multistream(true)
		r = gz
	}
	const tailSize = 4096
	buf := make([]byte, 0, 2*tailSize)
	chunk := make([]byte, 32*1024)
	for {
		n, err := r.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if len(buf) > tailSize {
			buf = append(buf[:0], buf[len(buf)-tailSize:]...)
		}
		if err != nil {
			// A truncated gzip stream (timeout) yields an error; only EOF counts as complete.
			return errors.Is(err, io.EOF) && bytes.Contains(buf, []byte(completeMarker))
		}
	}
}

// dropPageCache evicts path's pages from the page cache, which is charged to the container's
// memcg. Pages are synced first because POSIX_FADV_DONTNEED does not drop dirty pages.
func dropPageCache(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("fsync: %w", err)
	}
	if err := unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED); err != nil {
		return fmt.Errorf("fadvise: %w", err)
	}
	return nil
}

// rotate keeps the newest MaxReports reports. Names embed a sortable UTC timestamp.
func (c *Collector) rotate(dir string) {
	if c.cfg.MaxReports <= 0 {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var reports []string
	for _, e := range entries {
		if n := e.Name(); strings.HasPrefix(n, reportPrefix) && !e.IsDir() {
			reports = append(reports, n)
		}
	}
	slices.Sort(reports)
	slices.Reverse(reports)
	for i := c.cfg.MaxReports; i < len(reports); i++ {
		_ = os.Remove(filepath.Join(dir, reports[i]))
	}
}
