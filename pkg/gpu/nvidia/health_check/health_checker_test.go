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

package healthcheck

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/NVIDIA/gpu-monitoring-tools/bindings/go/nvml"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8sclienttesting "k8s.io/client-go/testing"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

func pointer[T any](s T) *T {
	return &s
}

type mockGPUDevice struct{}

func (gp *mockGPUDevice) parseMigDeviceUUID(UUID string) (string, uint, uint, error) {
	return UUID, 3173334309191009974, 1015241, nil
}

func TestCatchError(t *testing.T) {
	gp := mockGPUDevice{}
	device1 := pluginapi.Device{
		ID: "device1",
	}
	udevice1 := pluginapi.Device{
		ID:     "device1",
		Health: pluginapi.Unhealthy,
	}
	device2 := pluginapi.Device{
		ID: "device2",
	}
	udevice2 := pluginapi.Device{
		ID:     "device2",
		Health: pluginapi.Unhealthy,
	}
	tests := []struct {
		name             string
		event            nvml.Event
		hc               GPUHealthChecker
		wantErrorDevices []pluginapi.Device
	}{
		{
			name: "non-critical error",
			event: nvml.Event{
				UUID:              pointer("GPU-f053fce6-851c-1235-90ae-037069703604"),
				GpuInstanceId:     pointer(uint(3173334309191009974)),
				ComputeInstanceId: pointer(uint(1015241)),
				Etype:             0,
				Edata:             uint64(72),
			},
			hc: GPUHealthChecker{
				devices: map[string]pluginapi.Device{
					"device1": device1,
					"device2": device2,
				},
				nvmlDevices: map[string]*nvml.Device{
					"device1": {
						UUID: "GPU-f053fce6-851c-1235-90ae-037069703604",
					},
					"device2": {
						UUID: "GPU-f053fce6-851c-1235-90ae-037069703633",
					},
				},
				healthCriticalXid: map[uint64]bool{
					72: true,
					48: true,
				},
			},
			wantErrorDevices: []pluginapi.Device{},
		},
		{
			name: "xid error not included ",
			event: nvml.Event{
				UUID:              pointer("GPU-f053fce6-851c-1235-90ae-037069703604"),
				GpuInstanceId:     pointer(uint(3173334309191009974)),
				ComputeInstanceId: pointer(uint(1015241)),
				Etype:             nvml.XidCriticalError,
				Edata:             uint64(88),
			},
			hc: GPUHealthChecker{
				devices: map[string]pluginapi.Device{
					"device1": device1,
					"device2": device2,
				},
				nvmlDevices: map[string]*nvml.Device{
					"device1": {
						UUID: "GPU-f053fce6-851c-1235-90ae-037069703604",
					},
					"device2": {
						UUID: "GPU-f053fce6-851c-1235-90ae-037069703633",
					},
				},
				healthCriticalXid: map[uint64]bool{
					72: true,
					48: true,
				},
			},
			wantErrorDevices: []pluginapi.Device{},
		},
		{
			name: "catching xid 72",
			event: nvml.Event{
				UUID:              pointer("GPU-f053fce6-851c-1235-90ae-037069703604"),
				GpuInstanceId:     pointer(uint(3173334309191009974)),
				ComputeInstanceId: pointer(uint(1015241)),
				Etype:             nvml.XidCriticalError,
				Edata:             uint64(72),
			},
			hc: GPUHealthChecker{
				devices: map[string]pluginapi.Device{
					"device1": device1,
					"device2": device2,
				},
				nvmlDevices: map[string]*nvml.Device{
					"device1": {
						UUID: "GPU-f053fce6-851c-1235-90ae-037069703604",
					},
					"device2": {
						UUID: "GPU-f053fce6-851c-1235-90ae-037069703633",
					},
				},
				healthCriticalXid: map[uint64]bool{
					72: true,
					48: true,
				},
			},
			wantErrorDevices: []pluginapi.Device{udevice1},
		},
		{
			name: "unknown device",
			event: nvml.Event{
				UUID:              pointer("GPU-f053fce6-90ae-037069703604"),
				GpuInstanceId:     pointer(uint(3173334309191009974)),
				ComputeInstanceId: pointer(uint(1015241)),
				Etype:             nvml.XidCriticalError,
				Edata:             uint64(72),
			},
			hc: GPUHealthChecker{
				devices: map[string]pluginapi.Device{
					"device1": device1,
					"device2": device2,
				},
				nvmlDevices: map[string]*nvml.Device{
					"device1": {
						UUID: "GPU-f053fce6-851c-1235-90ae-037069703604",
					},
					"device2": {
						UUID: "GPU-f053fce6-851c-1235-90ae-037069703633",
					},
				},
				healthCriticalXid: map[uint64]bool{
					72: true,
					48: true,
				},
			},
			wantErrorDevices: []pluginapi.Device{},
		},
		{
			name: "not catching xid 72",
			event: nvml.Event{
				UUID:              pointer("GPU-f053fce6-851c-1235-90ae-037069703604"),
				GpuInstanceId:     pointer(uint(3173334309191009974)),
				ComputeInstanceId: pointer(uint(1015241)),
				Etype:             nvml.XidCriticalError,
				Edata:             uint64(72),
			},
			hc: GPUHealthChecker{
				devices: map[string]pluginapi.Device{
					"device1": device1,
				},
				nvmlDevices: map[string]*nvml.Device{
					"device1": {
						UUID: "GPU-f053fce6-851c-1235-90ae-037069703604",
					},
				},
				healthCriticalXid: map[uint64]bool{},
			},
			wantErrorDevices: []pluginapi.Device{},
		},
		{
			name: "catching all devices error",
			event: nvml.Event{
				UUID:              nil,
				GpuInstanceId:     pointer(uint(3173334309191009974)),
				ComputeInstanceId: pointer(uint(1015241)),
				Etype:             nvml.XidCriticalError,
				Edata:             uint64(48),
			},
			hc: GPUHealthChecker{
				devices: map[string]pluginapi.Device{
					"device1": device1,
					"device2": device2,
				},
				nvmlDevices: map[string]*nvml.Device{
					"device1": {
						UUID: "GPU-f053fce6-851c-1235-90ae-037069703604",
					},
					"device2": {
						UUID: "GPU-f053fce6-851c-1235-90ae-037069703633",
					},
				},
				healthCriticalXid: map[uint64]bool{
					72: true,
					48: true,
				},
			},
			wantErrorDevices: []pluginapi.Device{udevice1, udevice2},
		},
	}
	node := makeNode(nil, nil, nil)
	fakeClient := fake.NewSimpleClientset(&v1.NodeList{Items: []v1.Node{node}})

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.hc.kubeClient = fakeClient
			tt.hc.health = make(chan pluginapi.Device, len(tt.hc.devices))
			tt.hc.catchError(tt.event, &gp)
			gotErrorDevices := make(map[string]pluginapi.Device)
			for range tt.wantErrorDevices {
				if len(tt.hc.health) == 0 {
					t.Errorf("Fewer error devices was caught than expected.")
				} else {
					gotErrorDevice := <-tt.hc.health
					gotErrorDevices[gotErrorDevice.ID] = gotErrorDevice
				}
			}
			if len(tt.hc.health) != 0 {
				t.Errorf("More error devices was caught than expected.")
			}
			wantErrorDevicesMap := make(map[string]pluginapi.Device)
			for _, d := range tt.wantErrorDevices {
				wantErrorDevicesMap[d.ID] = d
			}

			if !reflect.DeepEqual(gotErrorDevices, wantErrorDevicesMap) {
				t.Errorf("Mismatched error devices. Got %v, want %v", gotErrorDevices, wantErrorDevicesMap)
			}
		})
	}
}

func TestUpdateLastHeartbeatTime(t *testing.T) {
	node := makeNode(nil, nil, nil)
	initialTime := metav1.Now()
	node.Status.Conditions = append(node.Status.Conditions, v1.NodeCondition{
		Type:               XIDConditionType,
		Status:             "True",
		LastHeartbeatTime:  initialTime,
		LastTransitionTime: initialTime,
		Reason:             "XID",
		Message:            node.Status.NodeInfo.BootID,
	})
	node.Status.Conditions = append(node.Status.Conditions, v1.NodeCondition{
		Type:   v1.NodeReady,
		Status: "True",
	})
	fakeClient := fake.NewSimpleClientset(&v1.NodeList{Items: []v1.Node{node}})

	hc := NewGPUHealthChecker(nil, nil, nil, fakeClient)
	hc.nodeName = "test-node"

	time.Sleep(2 * time.Second)
	hc.updateLastHeartbeatTime()
	updatedNode, _ := fakeClient.CoreV1().Nodes().Get(context.Background(), "test-node", metav1.GetOptions{})
	if updatedNode.Status.Conditions[0].LastHeartbeatTime == initialTime {
		t.Errorf("The XID condition HeartbeatTime was not updated")
	}
	if updatedNode.Status.Conditions[1] != node.Status.Conditions[1] {
		t.Errorf("The Ready condition was wrongly updated")
	}
}

func TestResetXIDConditionWithBackoff(t *testing.T) {
	// Initialize the node with condition
	node := makeNode(nil, nil, nil)
	initialTime := metav1.Now()
	node.Status.Conditions = append(node.Status.Conditions, v1.NodeCondition{
		Type:               XIDConditionType,
		Status:             "True",
		LastHeartbeatTime:  initialTime,
		LastTransitionTime: initialTime,
		Reason:             "XID",
		Message:            "0",
	})
	node.Status.NodeInfo.BootID = "1" // New boot ID to ensure condition is removed on success

	fakeClient := fake.NewSimpleClientset(&v1.NodeList{Items: []v1.Node{node}})

	callCount := 0
	// Mock the first 2 calls to fail, and the 3rd to succeed.
	fakeClient.Fake.PrependReactor("get", "nodes", func(action k8sclienttesting.Action) (handled bool, ret runtime.Object, err error) {
		callCount++
		if callCount < 3 {
			return true, nil, errors.New("fake API server error")
		}
		// On the 3rd call, we let the default reactor handle it, which will return the node.
		return false, nil, nil
	})

	hc := NewGPUHealthChecker(nil, nil, nil, fakeClient)
	hc.nodeName = "test-node"

	startTime := time.Now()
	hc.resetXIDConditionWithBackoff()
	duration := time.Since(startTime)

	if callCount != 3 {
		t.Errorf("Expected 3 calls to get node, but got %d", callCount)
	}

	// The first sleep is 1s, the second is 2s. Total should be > 3s.
	if duration < 3*time.Second {
		t.Errorf("Expected backoff to take at least 3 seconds, but it took %v", duration)
	}

	// Check that the condition is removed
	updatedNode, err := fakeClient.CoreV1().Nodes().Get(context.Background(), "test-node", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get node after backoff: %v", err)
	}

	conditionRemoved := true
	for _, c := range updatedNode.Status.Conditions {
		if c.Type == XIDConditionType {
			conditionRemoved = false
			break
		}
	}

	if !conditionRemoved {
		t.Errorf("XID condition was not removed after successful retry")
	}
}

func TestMonitorXidevent(t *testing.T) {
	for _, test := range []struct {
		desc                     string
		events                   []nvml.Event
		initialConditions        []v1.NodeCondition
		expectedLength           int
		expectedConditionType    v1.NodeConditionType
		expectedConditionStatus  v1.ConditionStatus
		expectedConditionReason  string
		expectedConditionMessage string
	}{
		{
			desc: "XID not in attention set",
			events: []nvml.Event{
				{
					Edata: uint64(72),
				},
			},
			expectedLength: 0,
		},
		{
			desc: "XID all in attention set",
			events: []nvml.Event{
				{
					Edata: uint64(79),
				},
				{
					Edata: uint64(123),
				},
			},
			expectedLength:           1,
			expectedConditionType:    XIDConditionType,
			expectedConditionStatus:  "True",
			expectedConditionReason:  "{\"123\":true,\"79\":true}",
			expectedConditionMessage: "123456",
		},
		{
			desc: "XID partially in attention set",
			events: []nvml.Event{
				{
					Edata: uint64(72),
				},
				{
					Edata: uint64(140),
				},
			},
			expectedLength:           1,
			expectedConditionType:    XIDConditionType,
			expectedConditionStatus:  "True",
			expectedConditionReason:  "{\"140\":true}",
			expectedConditionMessage: "123456",
		},
		{
			desc: "repetitive XID",
			events: []nvml.Event{
				{
					Edata: uint64(72),
				},
				{
					Edata: uint64(140),
				},
				{
					Edata: uint64(123),
				},
			},
			expectedLength:           1,
			expectedConditionType:    XIDConditionType,
			expectedConditionStatus:  "True",
			expectedConditionReason:  "{\"123\":true,\"140\":true}",
			expectedConditionMessage: "123456",
		},
	} {
		node := makeNode(nil, nil, test.initialConditions)
		node.Status.NodeInfo.BootID = "123456"
		fakeClient := fake.NewSimpleClientset(&v1.NodeList{Items: []v1.Node{node}})

		hc := NewGPUHealthChecker(nil, nil, nil, fakeClient)
		hc.nodeName = "test-node"

		for _, event := range test.events {
			hc.monitorXidevent(event)
		}
		updatedNode, _ := fakeClient.CoreV1().Nodes().Get(context.Background(), "test-node", metav1.GetOptions{})
		if len(updatedNode.Status.Conditions) != test.expectedLength || len(updatedNode.Status.Conditions) > 1 {
			t.Errorf("Expect condition length to have value %v, got %v", test.expectedLength, len(updatedNode.Status.Conditions))
		}
		if len(updatedNode.Status.Conditions) != 0 {
			condition := updatedNode.Status.Conditions[0]
			if condition.Type != test.expectedConditionType {
				t.Errorf("Expect condition.Type to have value %v, got %v", test.expectedConditionType, condition.Type)
			}
			if condition.Status != test.expectedConditionStatus {
				t.Errorf("Expect condition.Status to have value %v, got %v", test.expectedConditionStatus, condition.Status)
			}
			if condition.Reason != test.expectedConditionReason {
				t.Errorf("Expect condition.Reason to have value %v, got %v", test.expectedConditionReason, condition.Reason)
			}
			if condition.Message != test.expectedConditionMessage {
				t.Errorf("Expect condition.Message to have value %v, got %v", test.expectedConditionMessage, condition.Message)
			}
		}
	}

}

func makeNode(labels map[string]string, annotations map[string]string, conditions []v1.NodeCondition) v1.Node {
	metadata := metav1.ObjectMeta{
		Name: "test-node",
	}
	node := v1.Node{
		ObjectMeta: metadata,
	}
	if labels != nil {
		node.ObjectMeta.Labels = labels
	}
	if annotations != nil {
		node.ObjectMeta.Annotations = annotations
	}
	if conditions != nil {
		node.Status.Conditions = conditions
	}
	return node
}
