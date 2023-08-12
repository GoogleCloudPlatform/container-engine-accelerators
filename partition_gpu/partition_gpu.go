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
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/golang/glog"
)

var (
	nvidiaSmiPath = flag.String("nvidia-smi-path", "/usr/local/nvidia/bin/nvidia-smi", "Path where nvidia-smi is installed.")
	gpuConfigFile = flag.String("gpu-config", "/etc/nvidia/gpu_config.json", "File with GPU configurations for device plugin")
)

var partitionSizeToProfileID = map[string]string{
	//nvidia-tesla-a100
	"1g.5gb":  "19",
	"2g.10gb": "14",
	"3g.20gb": "9",
	"4g.20gb": "5",
	"7g.40gb": "0",
	//nvidia-a100-80gb, nvidia-h100-80gb
	"1g.10gb": "19",
	"2g.20gb": "14",
	"3g.40gb": "9",
	"4g.40gb": "5",
	"7g.80gb": "0",
	//nvidia-h100-80gb
	"1g.20gb": "15",
}

var partitionSizeMaxCount = map[string]int{
	//nvidia-tesla-a100
	"1g.5gb":  7,
	"2g.10gb": 3,
	"3g.20gb": 2,
	"4g.20gb": 1,
	"7g.40gb": 1,
	//nvidia-a100-80gb, nvidia-h100-80gb
	"1g.10gb": 7,
	"2g.20gb": 3,
	"3g.40gb": 2,
	"4g.40gb": 1,
	"7g.80gb": 1,
	//nvidia-h100-80gb
	"1g.20gb": 4,
}

const (
	SIGRTMIN       = 34
	Nvidia80gbH100 = "NVIDIA H100 80GB HBM3" //nvidia-h100-80gb
	Nvidia40gbA100 = "NVIDIA A100-SXM4-40GB" //nvidia-tesla-a100
	Nvidia80gbA100 = "NVIDIA A100-SXM4-80GB" //nvidia-a100-80gb
)

// GPUConfig stores the settings used to configure the GPUs on a node.
type GPUConfig struct {
	GPUPartitionSize string
}

func main() {
	flag.Parse()

	if _, err := os.Stat(*gpuConfigFile); os.IsNotExist(err) {
		glog.Infof("No GPU config file given, nothing to do.")
		return
	}
	gpuConfig, err := parseGPUConfig(*gpuConfigFile)
	if err != nil {
		glog.Infof("failed to parse GPU config file, taking no action.")
		return
	}
	glog.Infof("Using gpu config: %v", gpuConfig)
	if gpuConfig.GPUPartitionSize == "" {
		glog.Infof("No GPU partitions are required, exiting")
		return
	}

	if _, err := os.Stat(*nvidiaSmiPath); os.IsNotExist(err) {
		glog.Errorf("nvidia-smi path %s not found: %v", *nvidiaSmiPath, err)
		os.Exit(1)
	}

	migModeEnabled, err := currentMigMode()
	if err != nil {
		glog.Errorf("Failed to check if MIG mode is enabled: %v", err)
		os.Exit(1)
	}
	if !migModeEnabled {
		glog.Infof("MIG mode is not enabled. Enabling now.")
		glog.Infof("Checking the GPU type now.")
		gpuType, err := checkGpuType()
		if err != nil {
			glog.Errorf("Failed to check GPU Type: %v", err)
			os.Exit(1)
		}
		glog.Infof("Got GPU type used: %s", gpuType)
		if err := enableMigMode(); err != nil {
			glog.Errorf("Failed to enable MIG mode: %v", err)
			os.Exit(1)
		}
		// On NVIDIA Ampere GPUs, when MIG mode is enabled, the driver will attempt to reset the GPU so that MIG mode can take effect.
		// Starting with the Hopper generation of GPUs, enabling MIG mode no longer requires a GPU reset to take effect.
		// See https://docs.nvidia.com/datacenter/tesla/mig-user-guide/#enable-mig-mode for more information
		if gpuType == Nvidia40gbA100 || gpuType == Nvidia80gbA100 {
			glog.Infof("Rebooting node to enable MIG mode")
			if err := rebootNode(); err != nil {
				glog.Errorf("Failed to trigger node reboot after enabling MIG mode: %v", err)
			}
			// Exit, since we cannot proceed until node has rebooted, for MIG changes to take effect on NVIDIA Ampere GPUs.
			os.Exit(1)
		}
	}

	glog.Infof("MIG mode is enabled on all GPUs, proceeding to create GPU partitions.")

	glog.Infof("Cleaning up any existing GPU partitions")
	if err := cleanupAllGPUPartitions(); err != nil {
		glog.Errorf("Failed to cleanup GPU partitions: %v", err)
		os.Exit(1)
	}

	glog.Infof("Creating new GPU partitions")
	if err := createGPUPartitions(gpuConfig.GPUPartitionSize); err != nil {
		glog.Errorf("Failed to create GPU partitions: %v", err)
		os.Exit(1)
	}

	glog.Infof("Running %s", *nvidiaSmiPath)
	out, err := exec.Command(*nvidiaSmiPath).Output()
	if err != nil {
		glog.Errorf("Failed to run nvidia-smi, output: %s, error: %v", string(out), err)
	}
	glog.Infof("Output:\n %s", string(out))

}

func parseGPUConfig(gpuConfigFile string) (GPUConfig, error) {
	var gpuConfig GPUConfig

	gpuConfigContent, err := ioutil.ReadFile(gpuConfigFile)
	if err != nil {
		return gpuConfig, fmt.Errorf("unable to read gpu config file %s: %v", gpuConfigFile, err)
	}

	if err = json.Unmarshal(gpuConfigContent, &gpuConfig); err != nil {
		return gpuConfig, fmt.Errorf("failed to parse GPU config file contents: %s, error: %v", gpuConfigContent, err)
	}
	return gpuConfig, nil
}

// currentMigMode returns whether mig mode is currently enabled all GPUs attached to this node.
func currentMigMode() (bool, error) {
	out, err := exec.Command(*nvidiaSmiPath, "--query-gpu=mig.mode.current", "--format=csv,noheader").Output()
	if err != nil {
		return false, err
	}
	if strings.HasPrefix(string(out), "Enabled") {
		return true, nil
	}
	if strings.HasPrefix(string(out), "Disabled") {
		return false, nil
	}
	return false, fmt.Errorf("nvidia-smi returned invalid output: %s", out)
}

// enableMigMode enables MIG mode on all GPUs attached to the node. Requires node restart to take effect.
func enableMigMode() error {
	return exec.Command(*nvidiaSmiPath, "-mig", "1").Run()
}

// checkGpuType checkes the GPU type used
func checkGpuType() (string, error) {
	gpuType, err := exec.Command(*nvidiaSmiPath, "--query-gpu=gpu_name", "--format=csv,noheader").Output()
	if err != nil {
		return "", err
	}
	switch {
	case strings.HasPrefix(string(gpuType), Nvidia80gbH100):
		return Nvidia80gbH100, nil
	case strings.HasPrefix(string(gpuType), Nvidia40gbA100):
		return Nvidia40gbA100, nil
	case strings.HasPrefix(string(gpuType), Nvidia80gbA100):
		return Nvidia80gbA100, nil
	}
	return "", fmt.Errorf("nvidia-smi returned invalid GPU type for MIG: %s", gpuType)
}

func rebootNode() error {
	// Gracefully reboot systemd: https://man7.org/linux/man-pages/man1/systemd.1.html#SIGNALS
	return syscall.Kill(1, SIGRTMIN+5)
}

func cleanupAllGPUPartitions() error {
	args := []string{"mig", "-dci"}
	glog.Infof("Running %s %s", *nvidiaSmiPath, strings.Join(args, " "))
	out, err := exec.Command(*nvidiaSmiPath, args...).Output()
	if err != nil && !strings.Contains(string(out), "No GPU instances found") {
		return fmt.Errorf("failed to destroy compute instance, nvidia-smi output: %s, error: %v ", string(out), err)
	}
	glog.Infof("Output:\n %s", string(out))

	args = []string{"mig", "-dgi"}
	glog.Infof("Running %s %s", *nvidiaSmiPath, strings.Join(args, " "))
	out, err = exec.Command(*nvidiaSmiPath, args...).Output()
	if err != nil && !strings.Contains(string(out), "No GPU instances found") {
		return fmt.Errorf("failed to destroy gpu instance, nvidia-smi output: %s, error: %v ", string(out), err)
	}
	glog.Infof("Output:\n %s", string(out))
	return nil
}

func createGPUPartitions(partitionSize string) error {
	p, err := buildPartitionStr(partitionSize)
	if err != nil {
		return err
	}

	args := []string{"mig", "-cgi", p}
	glog.Infof("Running %s %s", *nvidiaSmiPath, strings.Join(args, " "))
	out, err := exec.Command(*nvidiaSmiPath, args...).Output()
	if err != nil {
		return fmt.Errorf("failed to create GPU Instances: output: %s, error: %v", string(out), err)
	}
	glog.Infof("Output:\n %s", string(out))

	args = []string{"mig", "-cci"}
	glog.Infof("Running %s %s", *nvidiaSmiPath, strings.Join(args, " "))
	out, err = exec.Command(*nvidiaSmiPath, args...).Output()
	if err != nil {
		return fmt.Errorf("failed to create compute instances: output: %s, error: %v", string(out), err)
	}
	glog.Infof("Output:\n %s", string(out))

	return nil

}

func buildPartitionStr(partitionSize string) (string, error) {
	if partitionSize == "" {
		return "", nil
	}

	p, ok := partitionSizeToProfileID[partitionSize]
	if !ok {
		return "", fmt.Errorf("%s is not a valid partition size", partitionSize)
	}

	partitionStr := p
	for i := 1; i < partitionSizeMaxCount[partitionSize]; i++ {
		partitionStr += fmt.Sprintf(",%s", p)
	}

	return partitionStr, nil
}
