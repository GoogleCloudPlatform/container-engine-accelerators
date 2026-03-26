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

package version_visibility

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
	DriverVersionPrefix   = "cloud.google.com/cuda.driver-version"
	DriverVersionMajor    = DriverVersionPrefix + ".major"
	DriverVersionMinor    = DriverVersionPrefix + ".minor"
	DriverVersionRevision = DriverVersionPrefix + ".revision"
	DriverVersionFull     = DriverVersionPrefix + ".full"
)

// PublishDriverVersionAnnotations queries the NVIDIA driver version via NVML and publishes it as annotations to the Kubernetes Node object.
func PublishDriverVersionAnnotations(ctx context.Context, kubeClient kubernetes.Interface, nodeName string) error {
	version, ret := nvml.SystemGetDriverVersion()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("failed to get driver version from NVML: %v", nvml.ErrorString(ret))
	}

	glog.Infof("Found NVIDIA Driver Version: %s", version)
	return patchNodeAnnotations(ctx, kubeClient, nodeName, parseDriverAnnotations(version))
}

func parseDriverAnnotations(version string) map[string]string {
	annotations := map[string]string{
		DriverVersionFull: version,
	}

	parts := strings.Split(version, ".")
	if len(parts) >= 1 {
		annotations[DriverVersionMajor] = parts[0]
	}
	if len(parts) >= 2 {
		annotations[DriverVersionMinor] = parts[1]
	}
	if len(parts) >= 3 {
		annotations[DriverVersionRevision] = parts[2]
	}

	return annotations
}

func patchNodeAnnotations(ctx context.Context, kubeClient kubernetes.Interface, nodeName string, annotations map[string]string) error {
	if nodeName == "" {
		return fmt.Errorf("node name is empty")
	}

	nodeApplyConfiguration := corev1apply.Node(nodeName).
		WithAnnotations(annotations)

	glog.Infof("Applying node %s annotations using SSA: %v", nodeName, annotations)
	_, err := kubeClient.CoreV1().Nodes().Apply(
		ctx,
		nodeApplyConfiguration,
		metav1.ApplyOptions{FieldManager: "gpu-device-plugin", Force: true},
	)
	if err != nil {
		return fmt.Errorf("failed to apply node %s annotations: %v", nodeName, err)
	}

	return nil
}
