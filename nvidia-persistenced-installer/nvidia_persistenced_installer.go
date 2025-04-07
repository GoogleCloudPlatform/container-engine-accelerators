// Copyright 2017 Google Inc. All Rights Reserved.
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
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/golang/glog"
)

const (
	minUVMSupportedVersion = 550
)

var (
	containerPathPrefix = flag.String("container-path", "/usr/local/nvidia", "Path on the container that mounts host nvidia install directory")
	cgpuConfigFile      = flag.String("cgpu-config", "/etc/nvidia/confidential_node_type.txt", "File with Confidential Node Type used on Node")
	readyDelay          = flag.Int64("ready-delay-ms", 1000, "How much time to wait before setting GPU to ready state. Adding a delay helps to reduce the chances of a start up error.")
)

func main() {
	flag.Parse()
	ctx := context.Background()

	// Only run persistence daemon on confidential GPU nodes.
	enabled, err := checkConfidentialGPUEnablement(ctx)
	if err != nil {
		glog.ExitContextf(ctx, "parseCGPUConfig failed: %v", err)
	}

	if enabled {
		// This is necessary to be able to use nvidia smi from the container to set the GPU to a ready state.
		if err := updateContainerLdCache(); err != nil {
			glog.ExitContextf(ctx, "updateContainerLdCache failed: %v", err)
		}

		if err := enablePersistenceMode(ctx); err != nil {
			glog.ExitContextf(ctx, "failed to start persistence mode: %v", err)
		}
		// Add small delay before setting the ready state for consistency.
		// If the workload starts too close to when the persistence daemon has started sometimes there can be errors.
		time.Sleep(time.Duration(*readyDelay) * time.Millisecond)
		if err := setGPUReadyState(ctx); err != nil {
			glog.ExitContextf(ctx, "failed to set gpu to ready state: %v", err)
		}
	} else {
		glog.InfoContext(ctx, "Confidential GPU is NOT enabled, skipping nvidia persistenced enablement.")
		// Don't exit as this is intended for a side car which would cause it to restart infinitely.
	}

	// Need to keep the container running so that the nvidia persistence daemon can keep running.
	for {
		time.Sleep(5 * time.Minute)
	}
}

func enablePersistenceMode(ctx context.Context) error {
	glog.InfoContext(ctx, "Starting NVIDIA persistence daemon.")
	cmdArgs := []string{}
	if versionMajor, err := nvidiaVersionMajor(ctx); err != nil {
		return err
	} else if versionMajor >= minUVMSupportedVersion {
		// UVM persistence mode is only available starting at R550.
		cmdArgs = append(cmdArgs, "--uvm-persistence-mode")
		glog.InfoContext(ctx, "using --uvm-persistence-mode")
	}
	cmdArgs = append(cmdArgs, "--nvidia-cfg-path="+*containerPathPrefix+"/lib64")
	persistencedCMD := exec.Command(*containerPathPrefix+"/bin/nvidia-persistenced", cmdArgs...)
	if err := persistencedCMD.Run(); err != nil {
		return err
	}
	glog.InfoContext(ctx, "NVIDIA Persistence Mode Enabled.")
	return nil
}

func setGPUReadyState(ctx context.Context) error {
	gpuReadyCMD := exec.Command(*containerPathPrefix+"/bin/nvidia-smi", "conf-compute", "-srs", "1")
	if err := gpuReadyCMD.Run(); err != nil {
		return err
	}
	glog.InfoContext(ctx, "Confidential GPU is ready.")
	return nil
}

func updateContainerLdCache() error {
	f, err := os.Create("/etc/ld.so.conf.d/nvidia.conf")
	if err != nil {
		f.Close()
		return fmt.Errorf("failed to update ld cache: %w", err)
	}
	f.WriteString(*containerPathPrefix + "/lib64")
	f.Close()
	err = exec.Command("ldconfig").Run()
	if err != nil {
		return fmt.Errorf("failed to update ld cache: %w", err)
	}
	return nil
}

func getLoadedNVIDIAKernelModuleVersion(ctx context.Context, versionFilePath string) string {
	glog.InfoContextf(ctx, "Attempting to read nvidia gpu driver version from: %s", versionFilePath)
	content, err := os.ReadFile(versionFilePath)
	if err != nil {
		glog.ErrorContextf(ctx, "Failed to read version file: %v", err)
		return ""
	}
	contentStr := string(content)
	kernelModuleVersionPattern := regexp.MustCompile(`\d+\.\d+\.\d+`)
	kernelModuleVersion := kernelModuleVersionPattern.FindString(contentStr)
	glog.InfoContextf(ctx, "nvidia gpu driver version: %s", kernelModuleVersion)
	return kernelModuleVersion
}

func nvidiaVersionMajor(ctx context.Context) (int, error) {
	version := getLoadedNVIDIAKernelModuleVersion(ctx, "/proc/driver/nvidia/version")
	if version == "" {
		return 0, fmt.Errorf("failed to read nvidia gpu driver version at /proc/driver/nvidia/version")
	}

	// Will be in this format as it was validated by the regex beforehand: 535.230.02
	before, _, found := strings.Cut(version, ".")
	if !found || len(before) != 3 {
		return 0, fmt.Errorf("invalid nvidia gpu driver version: %v", version)
	}

	versionMajor, err := strconv.Atoi(before)
	if err != nil {
		return 0, fmt.Errorf("invalid nvidia gpu driver version(%v), %w", version, err)
	}
	return versionMajor, nil
}

func checkConfidentialGPUEnablement(ctx context.Context) (bool, error) {
	file, err := os.ReadFile(*cgpuConfigFile)
	if err != nil {
		// Treat non existence of file as disabled.
		if os.IsNotExist(err) {
			glog.InfoContextf(ctx, "confidential node type file not found at %v, skipping persistenced installation", *cgpuConfigFile)
			return false, nil
		}
		return false, err
	}
	// Remove any trailing spaces and null strings to avoid issues in comparison.
	confidentialNodeType := strings.ToLower(strings.Trim(string(file), " \r\n\x00"))
	return confidentialNodeType == "tdx", nil
}
