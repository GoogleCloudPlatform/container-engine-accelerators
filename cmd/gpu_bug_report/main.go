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

// gpu-bug-report runs next to the NVIDIA GPU device plugin (same image, separate container)
// and collects NVIDIA bug reports from the host on request. It watches only its own Node.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia/diagnostics"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
)

var (
	nodeName   = flag.String("node-name", os.Getenv("NODE_NAME"), "Name of this node (defaults to $NODE_NAME).")
	hostRoot   = flag.String("host-root", "/root", "Container path where the host's / is mounted. The script runs chrooted here.")
	reportsDir = flag.String("reports-dir", "/home/kubernetes/bin/nvidia-bug-reports", "Host directory for reports. Must not be under the driver directory, which is mounted into GPU workloads.")
	driverDir  = flag.String("driver-dir", "/home/kubernetes/bin/nvidia", "Host driver install directory containing bin/nvidia-bug-report.sh.")
	timeout    = flag.Duration("timeout", 60*time.Second, "Maximum duration of one collection. Partial reports are kept.")
	maxReports = flag.Int("max-reports", 5, "Number of reports to keep.")
	maxLogAge  = flag.Duration("max-log-age", 7*24*time.Hour, "Maximum age of system journal logs to collect (0 disables the cap).")
	script     = flag.String("script", "", "Override the script path on the host (testing only).")
)

func main() {
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.LUTC)
	if *nodeName == "" {
		log.Fatal("--node-name or $NODE_NAME is required")
	}
	cleanReports := filepath.Clean(*reportsDir) + "/"
	cleanDriver := filepath.Clean(*driverDir) + "/"
	if strings.HasPrefix(cleanReports, cleanDriver) {
		log.Fatalf("--reports-dir %s must not be inside --driver-dir %s (it is exposed to GPU workloads)", *reportsDir, *driverDir)
	}

	candidates := []string{
		filepath.Join(*driverDir, "bin/nvidia-bug-report.sh"),
		"/opt/nvidia/bin/nvidia-bug-report.sh",
		"/usr/bin/nvidia-bug-report.sh",
	}
	if *script != "" {
		candidates = []string{*script}
	}

	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("kube config: %v", err)
	}
	config.ContentType = runtime.ContentTypeProtobuf
	config.UserAgent = "gpu-bug-report"
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("kube client: %v", err)
	}
	broadcaster := record.NewBroadcaster()
	sink := &trackingSink{EventSink: &typedcorev1.EventSinkImpl{Interface: client.CoreV1().Events("")}}
	// Flush pending events on exit, so a run interrupted by SIGTERM still reports GPUReportFailed.
	defer shutdownEvents(broadcaster, sink, 500*time.Millisecond, 5*time.Second)
	broadcaster.StartRecordingToSink(sink)
	recorder := broadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "gpu-bug-report", Host: *nodeName})

	collector := diagnostics.NewCollector(diagnostics.CollectorConfig{
		HostRoot:         *hostRoot,
		ReportsDir:       *reportsDir,
		ScriptCandidates: candidates,
		Timeout:          *timeout,
		MaxReports:       *maxReports,
		MaxLogAge:        *maxLogAge,
		Log:              os.Stderr,
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	log.Printf("gpu-bug-report starting: node=%s host-root=%s reports-dir=%s timeout=%v max-log-age=%v", *nodeName, *hostRoot, *reportsDir, *timeout, *maxLogAge)
	diagnostics.NewController(client, recorder, collector, *nodeName).Run(ctx)
	log.Print("gpu-bug-report stopped")
}
