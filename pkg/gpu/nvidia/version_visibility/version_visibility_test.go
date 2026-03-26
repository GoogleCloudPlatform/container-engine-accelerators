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
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestParseDriverAnnotations(t *testing.T) {
	tests := []struct {
		name     string
		version  string
		expected map[string]string
	}{
		{
			name:    "Full version",
			version: "550.107.02",
			expected: map[string]string{
				DriverVersionFull:     "550.107.02",
				DriverVersionMajor:    "550",
				DriverVersionMinor:    "107",
				DriverVersionRevision: "02",
			},
		},
		{
			name:    "Major only",
			version: "550",
			expected: map[string]string{
				DriverVersionFull:  "550",
				DriverVersionMajor: "550",
			},
		},
		{
			name:    "Major and Minor",
			version: "550.107",
			expected: map[string]string{
				DriverVersionFull:  "550.107",
				DriverVersionMajor: "550",
				DriverVersionMinor: "107",
			},
		},
		{
			name:    "Extra parts",
			version: "550.107.02.04",
			expected: map[string]string{
				DriverVersionFull:     "550.107.02.04",
				DriverVersionMajor:    "550",
				DriverVersionMinor:    "107",
				DriverVersionRevision: "02",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			annotations := parseDriverAnnotations(tc.version)
			if len(annotations) != len(tc.expected) {
				t.Errorf("expected %d annotations, got %d", len(tc.expected), len(annotations))
			}
			for k, v := range tc.expected {
				if annotations[k] != v {
					t.Errorf("expected annotation %s=%s, got %s", k, v, annotations[k])
				}
			}
		})
	}
}

func TestPatchNodeAnnotations(t *testing.T) {
	nodeName := "test-node"
	kubeClient := fake.NewSimpleClientset()

	// Create initial node
	_, err := kubeClient.CoreV1().Nodes().Create(context.Background(), &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
			Annotations: map[string]string{
				"existing-label": "value",
				"cloud.google.com/cuda.driver-version.major": "510",
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create node: %v", err)
	}

	annotations := map[string]string{
		"cloud.google.com/cuda.driver-version.major": "550",
		"cloud.google.com/cuda.driver-version.full":  "550.107.02",
	}

	err = patchNodeAnnotations(context.Background(), kubeClient, nodeName, annotations)
	if err != nil {
		t.Fatalf("patchNodeAnnotations failed: %v", err)
	}

	// Verify node annotations
	node, err := kubeClient.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get node: %v", err)
	}

	for k, v := range annotations {
		if node.Annotations[k] != v {
			t.Errorf("expected annotation %s=%s, got %s", k, v, node.Annotations[k])
		}
	}

	if node.Annotations["existing-label"] != "value" {
		t.Errorf("expected existing annotation to be preserved")
	}
}
