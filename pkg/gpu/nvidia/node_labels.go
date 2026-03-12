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

package nvidia

import (
	"context"
	"fmt"
	"strings"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/golang/glog"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1apply "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	NodeLabelsPrefix           = "cloud.google.com/cuda.driver-version"
	DriverVersionMajorLabel    = NodeLabelsPrefix + ".major"
	DriverVersionMinorLabel    = NodeLabelsPrefix + ".minor"
	DriverVersionRevisionLabel = NodeLabelsPrefix + ".revision"
	DriverVersionFullLabel     = NodeLabelsPrefix + ".full"
)

// PublishDriverVersionLabels queries the NVIDIA driver version via NVML and publishes it as labels to the Kubernetes Node object.
func PublishDriverVersionLabels(kubeClient kubernetes.Interface, nodeName string) error {
	version, ret := nvml.SystemGetDriverVersion()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("failed to get driver version from NVML: %v", nvml.ErrorString(ret))
	}

	glog.Infof("Found NVIDIA Driver Version: %s", version)
	return patchNodeLabels(kubeClient, nodeName, parseDriverVersion(version))
}

func parseDriverVersion(version string) map[string]string {
	labels := map[string]string{
		DriverVersionFullLabel: version,
	}

	parts := strings.Split(version, ".")
	if len(parts) >= 1 {
		labels[DriverVersionMajorLabel] = parts[0]
	}
	if len(parts) >= 2 {
		labels[DriverVersionMinorLabel] = parts[1]
	}
	if len(parts) >= 3 {
		labels[DriverVersionRevisionLabel] = parts[2]
	}

	return labels
}

func patchNodeLabels(kubeClient kubernetes.Interface, nodeName string, labels map[string]string) error {
	if nodeName == "" {
		return fmt.Errorf("node name is empty")
	}

	nodeApplyConfiguration := corev1apply.Node(nodeName).
		WithLabels(labels)

	glog.Infof("Applying node %s labels using SSA: %v", nodeName, labels)
	_, err := kubeClient.CoreV1().Nodes().Apply(
		context.Background(),
		nodeApplyConfiguration,
		metav1.ApplyOptions{FieldManager: "gpu-device-plugin", Force: true},
	)
	if err != nil {
		return fmt.Errorf("failed to apply node %s labels: %v", nodeName, err)
	}

	return nil
}
