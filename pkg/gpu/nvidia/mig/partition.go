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

package mig

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"github.com/golang/glog"
)

var partitionSizeToProfileID = map[string]string{
	// nvidia-tesla-a100
	"1g.5gb":  "19",
	"2g.10gb": "14",
	"3g.20gb": "9",
	"4g.20gb": "5",
	"7g.40gb": "0",
	// nvidia-a100-80gb, nvidia-h100-80gb
	"1g.10gb": "19",
	"2g.20gb": "14",
	"3g.40gb": "9",
	"4g.40gb": "5",
	"7g.80gb": "0",
	// nvidia-h100-80gb
	"1g.20gb": "15",

	// nvidia-h200-141gb
	"1g.18gb":  "19",
	"1g.35gb":  "15",
	"2g.35gb":  "14",
	"3g.71gb":  "9",
	"4g.71gb":  "5",
	"7g.141gb": "0",

	// nvidia-b200, nvidia-gb200
	"1g.23gb": "19",
	// nvidia-b200
	"1g.45gb":  "15",
	"2g.45gb":  "14",
	"3g.90gb":  "9",
	"4g.90gb":  "5",
	"7g.180gb": "0",

	// nvidia-gb200
	"1g.47gb":  "15",
	"2g.47gb":  "14",
	"3g.93gb":  "9",
	"4g.93gb":  "5",
	"7g.186gb": "0",

	// nvidia-rtx-pro-6000
	"1g.24gb":        "14",
	"1g.24gb+me":     "21",
	"1g.24gb+gfx":    "47",
	"1g.24gb+me.all": "65",
	"1g.24gb-me":     "67",
	"2g.48gb":        "5",
	"2g.48gb+gfx":    "35",
	"2g.48gb+me.all": "64",
	"2g.48gb-me":     "66",
	"4g.96gb":        "0",
	"4g.96gb+gfx":    "32",
}

var (
	lgiDataLineRegex   = regexp.MustCompile(`^\s*(\d+)\s+(MIG\s+[\w\.\+\-]+)\s+(\d+)\s+(\d+)\s+([\d:]+)\s*$`)
	explicitInstRegex  = regexp.MustCompile(`nvidia\d+/gi\d+(?:/ci\d+)?`)
	gpuInstanceIDRegex = regexp.MustCompile(`(?i)gpu\s+instance(?:\s+id)?\s*:?\s*(\d+)`)
	ciInstanceIDRegex  = regexp.MustCompile(`(?i)compute\s+instance(?:\s+id)?\s*:?\s*(\d+)`)
	gpuIDInTextRegex   = regexp.MustCompile(`(?i)\bgpu\s+(\d+)\b`)
	shortGIRegex       = regexp.MustCompile(`(?i)\b(gi\d+(?:/ci\d+)?)\b`)
	shortCIRegex       = regexp.MustCompile(`(?i)\b(ci\d+)\b`)
)

// SMIFunc executes nvidia-smi with the given arguments and returns its output.
type SMIFunc func(ctx context.Context, args ...string) ([]byte, error)

// NewSMIFunc returns an SMIFunc that runs the nvidia-smi binary at binaryPath.
func NewSMIFunc(binaryPath string) SMIFunc {
	return func(ctx context.Context, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, binaryPath, args...).CombinedOutput()
	}
}

// InstanceInUseError is returned when nvidia-smi mig -dci or -dgi refuses to
// destroy an instance because it is still in use by another client.
type InstanceInUseError struct {
	GPU      string
	Instance string
	Output   string
}

func (e *InstanceInUseError) Error() string {
	if e.Instance != "" {
		return fmt.Sprintf("mig instance %s on gpu %s is in use by another client: %s", e.Instance, e.GPU, e.Output)
	}
	return fmt.Sprintf("mig instance on gpu %s is in use by another client: %s", e.GPU, e.Output)
}

// PartitionMaxCount returns the expected number of partitions per GPU for a partition size.
func PartitionMaxCount(partitionSize string) (int, bool) {
	c, ok := gpuPartitionSizeMaxCount[partitionSize]
	return c, ok
}

// BuildPartitionStr builds the comma-separated profile ID argument for nvidia-smi mig -cgi.
func BuildPartitionStr(partitionSize string) (string, error) {
	if partitionSize == "" {
		return "", nil
	}

	p, ok := partitionSizeToProfileID[partitionSize]
	if !ok {
		return "", fmt.Errorf("%s is not a valid partition size", partitionSize)
	}

	maxCount, ok := gpuPartitionSizeMaxCount[partitionSize]
	if !ok {
		return "", fmt.Errorf("%s is not a valid partition size", partitionSize)
	}

	partitionStr := p
	for i := 1; i < maxCount; i++ {
		partitionStr += fmt.Sprintf(",%s", p)
	}

	return partitionStr, nil
}

func withGPU(args []string, targetGPU string) []string {
	if targetGPU == "" {
		return args
	}
	return append(args, "-i", targetGPU)
}

func isNoInstancesOutput(out string) bool {
	lower := strings.ToLower(out)
	return strings.Contains(lower, "no gpu instances found") ||
		strings.Contains(lower, "no compute instances found")
}

func gpuPrefixForInstance(targetGPU, out string) string {
	if strings.HasPrefix(targetGPU, "nvidia") {
		return strings.TrimPrefix(targetGPU, "nvidia")
	}
	if targetGPU != "" && !strings.Contains(targetGPU, ":") {
		return targetGPU
	}
	if m := gpuIDInTextRegex.FindStringSubmatch(out); len(m) == 2 {
		return m[1]
	}
	return ""
}

func parseInUseInstance(out, targetGPU string) (string, bool) {
	lower := strings.ToLower(out)
	if !strings.Contains(lower, "in use") {
		return "", false
	}

	if m := explicitInstRegex.FindString(out); m != "" {
		return m, true
	}

	gpu := gpuPrefixForInstance(targetGPU, out)

	giMatch := gpuInstanceIDRegex.FindStringSubmatch(out)
	ciMatch := ciInstanceIDRegex.FindStringSubmatch(out)
	if len(giMatch) == 2 {
		inst := fmt.Sprintf("gi%s", giMatch[1])
		if gpu != "" {
			inst = fmt.Sprintf("nvidia%s/%s", gpu, inst)
		}
		if len(ciMatch) == 2 {
			inst = fmt.Sprintf("%s/ci%s", inst, ciMatch[1])
		}
		return inst, true
	}
	if len(ciMatch) == 2 {
		inst := fmt.Sprintf("ci%s", ciMatch[1])
		if gpu != "" {
			inst = fmt.Sprintf("nvidia%s/%s", gpu, inst)
		}
		return inst, true
	}
	if m := shortGIRegex.FindStringSubmatch(out); len(m) == 2 {
		inst := strings.ToLower(m[1])
		if gpu != "" {
			inst = fmt.Sprintf("nvidia%s/%s", gpu, inst)
		}
		return inst, true
	}
	if m := shortCIRegex.FindStringSubmatch(out); len(m) == 2 {
		inst := strings.ToLower(m[1])
		if gpu != "" {
			inst = fmt.Sprintf("nvidia%s/%s", gpu, inst)
		}
		return inst, true
	}

	if gpu != "" {
		return "nvidia" + gpu, true
	}
	if targetGPU != "" {
		return targetGPU, true
	}
	return "unknown", true
}

// DestroyPartitions destroys compute instances (-dci) and GPU instances (-dgi).
// When targetGPU is non-empty (e.g. a PCI bus ID), -i <targetGPU> is passed to
// scope the operation to that GPU. It is idempotent when no instances exist. If
// an instance is in use by another client, it returns *InstanceInUseError.
func DestroyPartitions(ctx context.Context, smi SMIFunc, targetGPU string) error {
	dciArgs := withGPU([]string{"mig", "-dci"}, targetGPU)
	glog.Infof("Running nvidia-smi %s", strings.Join(dciArgs, " "))
	out, err := smi(ctx, dciArgs...)
	outStr := string(out)
	if err != nil && !isNoInstancesOutput(outStr) {
		if inst, ok := parseInUseInstance(outStr, targetGPU); ok {
			return &InstanceInUseError{GPU: targetGPU, Instance: inst, Output: strings.TrimSpace(outStr)}
		}
		return fmt.Errorf("failed to destroy compute instance, nvidia-smi output: %s, error: %w", outStr, err)
	}
	glog.Infof("Output:\n %s", outStr)

	dgiArgs := withGPU([]string{"mig", "-dgi"}, targetGPU)
	glog.Infof("Running nvidia-smi %s", strings.Join(dgiArgs, " "))
	out, err = smi(ctx, dgiArgs...)
	outStr = string(out)
	if err != nil && !isNoInstancesOutput(outStr) {
		if inst, ok := parseInUseInstance(outStr, targetGPU); ok {
			return &InstanceInUseError{GPU: targetGPU, Instance: inst, Output: strings.TrimSpace(outStr)}
		}
		return fmt.Errorf("failed to destroy gpu instance, nvidia-smi output: %s, error: %w", outStr, err)
	}
	glog.Infof("Output:\n %s", outStr)
	return nil
}

// CreatePartitions creates GPU instances (-cgi) and compute instances (-cci)
// for partitionSize. When targetGPU is non-empty, -i <targetGPU> is passed.
func CreatePartitions(ctx context.Context, smi SMIFunc, targetGPU, partitionSize string) error {
	p, err := BuildPartitionStr(partitionSize)
	if err != nil {
		return err
	}

	cgiArgs := withGPU([]string{"mig", "-cgi", p}, targetGPU)
	glog.Infof("Running nvidia-smi %s", strings.Join(cgiArgs, " "))
	out, err := smi(ctx, cgiArgs...)
	if err != nil {
		return fmt.Errorf("failed to create GPU Instances: output: %s, error: %w", string(out), err)
	}
	glog.Infof("Output:\n %s", string(out))

	cciArgs := withGPU([]string{"mig", "-cci"}, targetGPU)
	glog.Infof("Running nvidia-smi %s", strings.Join(cciArgs, " "))
	out, err = smi(ctx, cciArgs...)
	if err != nil {
		return fmt.Errorf("failed to create compute instances: output: %s, error: %w", string(out), err)
	}
	glog.Infof("Output:\n %s", string(out))

	return nil
}

// ListPartitions runs nvidia-smi mig -lgi (with -i <targetGPU> when non-empty)
// and parses the output into a map of GPU index -> slice of profile IDs.
func ListPartitions(ctx context.Context, smi SMIFunc, targetGPU string) (map[string][]string, bool, error) {
	lgiArgs := withGPU([]string{"mig", "-lgi"}, targetGPU)
	out, err := smi(ctx, lgiArgs...)
	if err != nil {
		return nil, false, fmt.Errorf("failed to execute 'nvidia-smi %s': %w", strings.Join(lgiArgs, " "), err)
	}
	glog.Infof("Output:\n %s", string(out))
	return ParseLGIOutput(string(out))
}

// ParseLGIOutput parses the table output of 'nvidia-smi mig -lgi' and returns
// a map of GPU index -> profile IDs and whether all instances share the same profile ID.
func ParseLGIOutput(lgiOutput string) (map[string][]string, bool, error) {
	gpuProfileIDsMap := make(map[string][]string)
	profileIDFirst := ""

	scanner := bufio.NewScanner(strings.NewReader(lgiOutput))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if strings.Contains(line, "====") {
			continue
		}
		if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") {
			continue
		}

		contentInPipes := strings.TrimSpace(line[1 : len(line)-1])
		matches := lgiDataLineRegex.FindStringSubmatch(contentInPipes)

		if len(matches) == 6 {
			gpuIndex := matches[1]
			profileID := matches[3]
			if profileIDFirst == "" {
				profileIDFirst = profileID
			} else if profileIDFirst != profileID {
				return gpuProfileIDsMap, false, nil
			}
			gpuProfileIDsMap[gpuIndex] = append(gpuProfileIDsMap[gpuIndex], profileID)
			glog.Infof("Parsed GI on GPU %s: Profile ID %s", gpuIndex, profileID)
		} else if contentInPipes != "" {
			glog.Infof("Line did not match expected GI data format: '%s' (Content: '%s')", line, contentInPipes)
		}
	}

	if err := scanner.Err(); err != nil {
		return gpuProfileIDsMap, false, err
	}

	if len(gpuProfileIDsMap) == 0 {
		return gpuProfileIDsMap, false, nil
	}
	glog.Infof("map: %v", gpuProfileIDsMap)
	return gpuProfileIDsMap, true, nil
}

// CheckDesired reports whether every GPU in partitions has desiredMaxCount instances.
func CheckDesired(partitions map[string][]string, desiredMaxCount int) bool {
	if len(partitions) == 0 {
		return false
	}

	for _, profileIDsOnGpu := range partitions {
		if len(profileIDsOnGpu) != desiredMaxCount {
			return false
		}
	}

	return true
}

// CheckCurrentPartitionProfileCounts checks whether the active MIG GPU instances
// match desiredMaxCount across the queried GPUs.
func CheckCurrentPartitionProfileCounts(ctx context.Context, smi SMIFunc, targetGPU string, desiredMaxCount int) bool {
	partitions, uniform, err := ListPartitions(ctx, smi, targetGPU)
	if err != nil {
		glog.Errorf("failed to list partitions: %v", err)
		return false
	}
	if !uniform {
		glog.Info("Partitions are not uniform, partition reconstruction needed.")
		return false
	}
	if targetGPU != "" && len(partitions) != 1 {
		return false
	}
	return CheckDesired(partitions, desiredMaxCount)
}

// IsGPUPartitioned checks whether a specific GPU (or all listed GPUs when
// targetGPU is empty) already has the exact profile ID and instance count for
// partitionSize. When targetGPU is non-empty, -i <targetGPU> scopes -lgi to
// that single GPU regardless of its 0-based NVML enumeration index.
func IsGPUPartitioned(ctx context.Context, smi SMIFunc, targetGPU, partitionSize string) bool {
	desiredProfileID, ok := partitionSizeToProfileID[partitionSize]
	if !ok {
		return false
	}
	desiredMaxCount, ok := gpuPartitionSizeMaxCount[partitionSize]
	if !ok {
		return false
	}
	partitions, uniform, err := ListPartitions(ctx, smi, targetGPU)
	if err != nil || !uniform {
		return false
	}
	if targetGPU != "" && len(partitions) != 1 {
		return false
	}
	if !CheckDesired(partitions, desiredMaxCount) {
		return false
	}
	for _, profiles := range partitions {
		for _, p := range profiles {
			if p != desiredProfileID {
				return false
			}
		}
	}
	return true
}

// ConvergePartitions brings the target GPU (or all GPUs when targetGPU is empty)
// to the desired partitionSize. It is idempotent: if the current partitions
// already match partitionSize, no changes are made.
func ConvergePartitions(ctx context.Context, smi SMIFunc, targetGPU, partitionSize string) error {
	if partitionSize == "" {
		return nil
	}
	if _, err := BuildPartitionStr(partitionSize); err != nil {
		return err
	}
	if IsGPUPartitioned(ctx, smi, targetGPU, partitionSize) {
		glog.Infof("Current GPU partition configuration matches the desired state (%s). No changes needed.", partitionSize)
		return nil
	}
	glog.Infof("Current GPU partition configuration does not match the desired state (%s). Reconfiguring partitions...", partitionSize)
	if err := DestroyPartitions(ctx, smi, targetGPU); err != nil {
		return err
	}
	return CreatePartitions(ctx, smi, targetGPU, partitionSize)
}
