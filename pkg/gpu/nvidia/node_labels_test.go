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
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestParseDriverVersion(t *testing.T) {
	tests := []struct {
		name     string
		version  string
		expected map[string]string
	}{
		{
			name:    "Full version",
			version: "550.107.02",
			expected: map[string]string{
				DriverVersionFullLabel:     "550.107.02",
				DriverVersionMajorLabel:    "550",
				DriverVersionMinorLabel:    "107",
				DriverVersionRevisionLabel: "02",
			},
		},
		{
			name:    "Major only",
			version: "550",
			expected: map[string]string{
				DriverVersionFullLabel:  "550",
				DriverVersionMajorLabel: "550",
			},
		},
		{
			name:    "Major and Minor",
			version: "550.107",
			expected: map[string]string{
				DriverVersionFullLabel:  "550.107",
				DriverVersionMajorLabel: "550",
				DriverVersionMinorLabel: "107",
			},
		},
		{
			name:    "Extra parts",
			version: "550.107.02.04",
			expected: map[string]string{
				DriverVersionFullLabel:     "550.107.02.04",
				DriverVersionMajorLabel:    "550",
				DriverVersionMinorLabel:    "107",
				DriverVersionRevisionLabel: "02",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			labels := parseDriverVersion(tc.version)
			if len(labels) != len(tc.expected) {
				t.Errorf("expected %d labels, got %d", len(tc.expected), len(labels))
			}
			for k, v := range tc.expected {
				if labels[k] != v {
					t.Errorf("expected label %s=%s, got %s", k, v, labels[k])
				}
			}
		})
	}
}

func TestPatchNodeLabels(t *testing.T) {
	nodeName := "test-node"
	kubeClient := fake.NewSimpleClientset()

	// Create initial node
	_, err := kubeClient.CoreV1().Nodes().Create(context.Background(), &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
			Labels: map[string]string{
				"existing-label": "value",
				"cloud.google.com/cuda.driver-version.major": "510",
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create node: %v", err)
	}

	labels := map[string]string{
		"cloud.google.com/cuda.driver-version.major": "550",
		"cloud.google.com/cuda.driver-version.full":  "550.107.02",
	}

	err = patchNodeLabels(kubeClient, nodeName, labels)
	if err != nil {
		t.Fatalf("patchNodeLabels failed: %v", err)
	}

	// Verify node labels
	node, err := kubeClient.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get node: %v", err)
	}

	for k, v := range labels {
		if node.Labels[k] != v {
			t.Errorf("expected label %s=%s, got %s", k, v, node.Labels[k])
		}
	}

	if node.Labels["existing-label"] != "value" {
		t.Errorf("expected existing label to be preserved")
	}
}
