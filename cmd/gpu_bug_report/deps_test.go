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
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestMinimalDependencies keeps the executor cgo-free and independent of the device plugin:
// it must never load NVML or talk to the kubelet device-plugin gRPC API.
func TestMinimalDependencies(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	cmd := exec.Command("go", "list", "-deps", ".")
	// Match the Dockerfile build; with cgo enabled, net would pull in runtime/cgo.
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	for _, dep := range strings.Fields(string(out)) {
		for _, banned := range []string{"google.golang.org/grpc", "github.com/NVIDIA/", "runtime/cgo"} {
			if strings.HasPrefix(dep, banned) {
				t.Errorf("gpu-bug-report must not depend on %s (found %s)", banned, dep)
			}
		}
	}
}
