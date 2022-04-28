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

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/golang/glog"
)

var (
	nvidiaSmiPath = flag.String("nvidia-smi-path", "/usr/local/nvidia/bin/nvidia-smi", "Path where nvidia-smi is installed.")
	gpuConfigFile = flag.String("gpu-config", "/etc/nvidia/gpu_config.json", "File with GPU configurations for device plugin")
)

const SIGRTMIN = 34

// GPUConfig stores the settings used to configure the GPUs on a node.
type GPUConfig struct {
	GPUPartitionSize string
}

type GPUAvailableProfiles struct {
	byname map[string]GPUProfile
}

type GPUProfile struct {
	id int
	instances_total int
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
		if err := enableMigMode(); err != nil {
			glog.Errorf("Failed to enable MIG mode: %v", err)
			os.Exit(1)
		}
		glog.Infof("Rebooting node to enable MIG mode")
		if err := rebootNode(); err != nil {
			glog.Errorf("Failed to trigger node reboot after enabling MIG mode: %v", err)
		}

		// Exit, since we cannot proceed until node has rebooted, for MIG changes to take effect.
		os.Exit(1)
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

// convert a nvml response byte array to a string
func _nvmlStrToString(rawstr[96] int8) (string) {
	ba := []byte{}
	for _, b := range(rawstr) {
		if b == 0 {
			return string(ba)
		}
		ba = append(ba, byte(b))
	}
	return string(ba)
}

// list all available profiles of the requested GPU (using NVML)
func ListGpuAvailableProfiles(gpu_index int)(GPUAvailableProfiles, error) {
	if err := nvml.Init(); err != nvml.SUCCESS {
		glog.Fatalf("failed to initialize nvml: %v", err)
	}
	defer nvml.Shutdown()

	profiles := GPUAvailableProfiles{ byname: make(map[string]GPUProfile) }

	device, ret := nvml.DeviceGetHandleByIndex(gpu_index)
	if ret != nvml.SUCCESS {
		return profiles, fmt.Errorf("error getting device info: %v", nvml.ErrorString(ret))
	}

	for profile_id := nvml.GPU_INSTANCE_PROFILE_1_SLICE; profile_id < nvml.GPU_INSTANCE_PROFILE_COUNT; profile_id++ {
		profile_v := nvml.DeviceGetGpuInstanceProfileInfoV(device, profile_id)
		profile, ret := profile_v.V2()
		if ret != nvml.SUCCESS {
			if ret == nvml.ERROR_NOT_SUPPORTED {
				continue
			}
			return profiles, fmt.Errorf("error getting profile info: %s", nvml.ErrorString(ret))
		}
		profile_name_raw := _nvmlStrToString(profile.Name)
		profile_name := strings.Replace(profile_name_raw, "MIG ", "", 1)
		profiles.byname[profile_name] = GPUProfile{
			id: int(profile.Id),
			instances_total: int(profile.InstanceCount),
		}
		glog.Infof("profile: gpu: %v, name: %-12.12s, id: %3v, instances total: %2v",
					gpu_index, profile_name, profile.Id, profile.InstanceCount)
	}

	return profiles, nil
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
	// currently only single-gpu systems are supported
	gpu_index := 0
	profiles, err := ListGpuAvailableProfiles(gpu_index)
	if err != nil {
		return err
	}

	p, err := buildPartitionStr(partitionSize, profiles)
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

func buildPartitionStr(partitionSize string, profiles GPUAvailableProfiles) (string, error) {
	if partitionSize == "" {
		return "", nil
	}

	p, ok := profiles.byname[partitionSize]
	if !ok {
		return "", fmt.Errorf("%s is not a valid partition size", partitionSize)
	}

	partitionStr := fmt.Sprint(p.id)
	for i := 1; i < p.instances_total; i++ {
		partitionStr += fmt.Sprintf(",%d", p.id)
	}

	return partitionStr, nil
}
