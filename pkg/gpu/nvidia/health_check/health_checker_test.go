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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8sclienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

type mockEventSet struct {
	events    chan nvmlEvent
	freeCount int32
	freeRet   nvml.Return
}

func newMockEventSet(buf int) *mockEventSet {
	return &mockEventSet{
		events:  make(chan nvmlEvent, buf),
		freeRet: nvml.SUCCESS,
	}
}

func (m *mockEventSet) Wait(timeoutMs uint32) (nvmlEvent, nvml.Return) {
	select {
	case e, ok := <-m.events:
		if !ok {
			return nvmlEvent{}, nvml.ERROR_TIMEOUT
		}
		return e, nvml.SUCCESS
	case <-time.After(time.Duration(timeoutMs) * time.Millisecond):
		return nvmlEvent{}, nvml.ERROR_TIMEOUT
	}
}

func (m *mockEventSet) Free() nvml.Return {
	atomic.AddInt32(&m.freeCount, 1)
	return m.freeRet
}

type mockDevice struct {
	uuid           string
	uuidRet        nvml.Return
	minor          int
	minorRet       nvml.Return
	migCurrentMode int
	migPendingMode int
	migModeRet     nvml.Return
	maxMigs        int
	maxMigsRet     nvml.Return
	migByIndex     map[int]nvmlDevice
	migByIndexRet  map[int]nvml.Return
	parent         nvmlDevice
	parentRet      nvml.Return
	gi             int
	giRet          nvml.Return
	ci             int
	ciRet          nvml.Return
	registerRet    nvml.Return
	registerCount  int32
}

func (m *mockDevice) GetUUID() (string, nvml.Return) {
	return m.uuid, m.uuidRet
}

func (m *mockDevice) GetMinorNumber() (int, nvml.Return) {
	return m.minor, m.minorRet
}

func (m *mockDevice) GetMigMode() (int, int, nvml.Return) {
	return m.migCurrentMode, m.migPendingMode, m.migModeRet
}

func (m *mockDevice) GetMaxMigDeviceCount() (int, nvml.Return) {
	return m.maxMigs, m.maxMigsRet
}

func (m *mockDevice) GetMigDeviceHandleByIndex(index int) (nvmlDevice, nvml.Return) {
	if m.migByIndexRet != nil {
		if ret, ok := m.migByIndexRet[index]; ok && ret != nvml.SUCCESS {
			return nil, ret
		}
	}
	if m.migByIndex != nil {
		if dev, ok := m.migByIndex[index]; ok {
			return dev, nvml.SUCCESS
		}
	}
	return nil, nvml.ERROR_NOT_FOUND
}

func (m *mockDevice) GetDeviceHandleFromMigDeviceHandle() (nvmlDevice, nvml.Return) {
	if m.parent == nil {
		if m.parentRet != nvml.SUCCESS {
			return nil, m.parentRet
		}
		return nil, nvml.ERROR_INVALID_ARGUMENT
	}
	return m.parent, m.parentRet
}

func (m *mockDevice) GetGpuInstanceId() (int, nvml.Return) {
	return m.gi, m.giRet
}

func (m *mockDevice) GetComputeInstanceId() (int, nvml.Return) {
	return m.ci, m.ciRet
}

func (m *mockDevice) RegisterEvents(eventTypes uint64, set nvmlEventSet) nvml.Return {
	atomic.AddInt32(&m.registerCount, 1)
	return m.registerRet
}

type mockNVML struct {
	mu             sync.Mutex
	devices        []nvmlDevice
	byUUID         map[string]nvmlDevice
	countRet       nvml.Return
	handleRet      map[int]nvml.Return
	eventSets      []*mockEventSet
	eventSetCreate func() (nvmlEventSet, nvml.Return)
}

func (m *mockNVML) DeviceGetCount() (int, nvml.Return) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.countRet != nvml.SUCCESS {
		return 0, m.countRet
	}
	return len(m.devices), nvml.SUCCESS
}

func (m *mockNVML) DeviceGetHandleByIndex(index int) (nvmlDevice, nvml.Return) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.handleRet != nil {
		if ret, ok := m.handleRet[index]; ok && ret != nvml.SUCCESS {
			return nil, ret
		}
	}
	if index < 0 || index >= len(m.devices) {
		return nil, nvml.ERROR_INVALID_ARGUMENT
	}
	return m.devices[index], nvml.SUCCESS
}

func (m *mockNVML) DeviceGetHandleByUUID(uuid string) (nvmlDevice, nvml.Return) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.byUUID != nil {
		if dev, ok := m.byUUID[uuid]; ok {
			return dev, nvml.SUCCESS
		}
	}
	return nil, nvml.ERROR_NOT_FOUND
}

func (m *mockNVML) EventSetCreate() (nvmlEventSet, nvml.Return) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.eventSetCreate != nil {
		return m.eventSetCreate()
	}
	es := newMockEventSet(16)
	m.eventSets = append(m.eventSets, es)
	return es, nvml.SUCCESS
}

type mockGPUDevice struct{}

func (gp *mockGPUDevice) parseMigDeviceUUID(UUID string) (string, uint32, uint32, error) {
	return UUID, 1, 2, nil
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
		event            nvmlEvent
		hc               GPUHealthChecker
		wantErrorDevices []pluginapi.Device
	}{
		{
			name: "non-critical error",
			event: nvmlEvent{
				EventType:         0,
				EventData:         72,
				DeviceUUID:        "GPU-f053fce6-851c-1235-90ae-037069703604",
				GpuInstanceID:     1,
				ComputeInstanceID: 2,
			},
			hc: GPUHealthChecker{
				devices: map[string]pluginapi.Device{
					"device1": device1,
					"device2": device2,
				},
				nvmlDevices: map[string]nvmlDevice{
					"device1": &mockDevice{uuid: "GPU-f053fce6-851c-1235-90ae-037069703604"},
					"device2": &mockDevice{uuid: "GPU-f053fce6-851c-1235-90ae-037069703633"},
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
			event: nvmlEvent{
				EventType:         nvml.EventTypeXidCriticalError,
				EventData:         88,
				DeviceUUID:        "GPU-f053fce6-851c-1235-90ae-037069703604",
				GpuInstanceID:     1,
				ComputeInstanceID: 2,
			},
			hc: GPUHealthChecker{
				devices: map[string]pluginapi.Device{
					"device1": device1,
					"device2": device2,
				},
				nvmlDevices: map[string]nvmlDevice{
					"device1": &mockDevice{uuid: "GPU-f053fce6-851c-1235-90ae-037069703604"},
					"device2": &mockDevice{uuid: "GPU-f053fce6-851c-1235-90ae-037069703633"},
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
			event: nvmlEvent{
				EventType:         nvml.EventTypeXidCriticalError,
				EventData:         72,
				DeviceUUID:        "GPU-f053fce6-851c-1235-90ae-037069703604",
				GpuInstanceID:     1,
				ComputeInstanceID: 2,
			},
			hc: GPUHealthChecker{
				devices: map[string]pluginapi.Device{
					"device1": device1,
					"device2": device2,
				},
				nvmlDevices: map[string]nvmlDevice{
					"device1": &mockDevice{uuid: "GPU-f053fce6-851c-1235-90ae-037069703604"},
					"device2": &mockDevice{uuid: "GPU-f053fce6-851c-1235-90ae-037069703633"},
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
			event: nvmlEvent{
				EventType:         nvml.EventTypeXidCriticalError,
				EventData:         72,
				DeviceUUID:        "GPU-f053fce6-90ae-037069703604",
				GpuInstanceID:     1,
				ComputeInstanceID: 2,
			},
			hc: GPUHealthChecker{
				devices: map[string]pluginapi.Device{
					"device1": device1,
					"device2": device2,
				},
				nvmlDevices: map[string]nvmlDevice{
					"device1": &mockDevice{uuid: "GPU-f053fce6-851c-1235-90ae-037069703604"},
					"device2": &mockDevice{uuid: "GPU-f053fce6-851c-1235-90ae-037069703633"},
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
			event: nvmlEvent{
				EventType:         nvml.EventTypeXidCriticalError,
				EventData:         72,
				DeviceUUID:        "GPU-f053fce6-851c-1235-90ae-037069703604",
				GpuInstanceID:     1,
				ComputeInstanceID: 2,
			},
			hc: GPUHealthChecker{
				devices: map[string]pluginapi.Device{
					"device1": device1,
				},
				nvmlDevices: map[string]nvmlDevice{
					"device1": &mockDevice{uuid: "GPU-f053fce6-851c-1235-90ae-037069703604"},
				},
				healthCriticalXid: map[uint64]bool{},
			},
			wantErrorDevices: []pluginapi.Device{},
		},
		{
			name: "catching all devices error",
			event: nvmlEvent{
				EventType:         nvml.EventTypeXidCriticalError,
				EventData:         48,
				DeviceUUID:        "",
				GpuInstanceID:     1,
				ComputeInstanceID: 2,
			},
			hc: GPUHealthChecker{
				devices: map[string]pluginapi.Device{
					"device1": device1,
					"device2": device2,
				},
				nvmlDevices: map[string]nvmlDevice{
					"device1": &mockDevice{uuid: "GPU-f053fce6-851c-1235-90ae-037069703604"},
					"device2": &mockDevice{uuid: "GPU-f053fce6-851c-1235-90ae-037069703633"},
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

	for i := range tests {
		tt := &tests[i]
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
	initialTime := metav1.NewTime(time.Now().Add(-1 * time.Minute))
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

	hc.updateLastHeartbeatTime()
	updatedNode, err := fakeClient.CoreV1().Nodes().Get(context.Background(), "test-node", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get updated node: %v", err)
	}
	if !updatedNode.Status.Conditions[0].LastHeartbeatTime.After(initialTime.Time) {
		t.Errorf("The XID condition HeartbeatTime was not updated: got %v, initial %v", updatedNode.Status.Conditions[0].LastHeartbeatTime, initialTime)
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

	stopCh := make(chan struct{})
	defer close(stopCh)

	startTime := time.Now()
	hc.resetXIDConditionWithBackoff(stopCh)
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
		events                   []uint64
		initialConditions        []v1.NodeCondition
		expectedLength           int
		expectedConditionType    v1.NodeConditionType
		expectedConditionStatus  v1.ConditionStatus
		expectedConditionReason  string
		expectedConditionMessage string
	}{
		{
			desc:           "XID not in attention set",
			events:         []uint64{72},
			expectedLength: 0,
		},
		{
			desc:                     "XID all in attention set",
			events:                   []uint64{79, 123},
			expectedLength:           1,
			expectedConditionType:    XIDConditionType,
			expectedConditionStatus:  "True",
			expectedConditionReason:  "{\"123\":true,\"79\":true}",
			expectedConditionMessage: "123456",
		},
		{
			desc:                     "XID partially in attention set",
			events:                   []uint64{72, 140},
			expectedLength:           1,
			expectedConditionType:    XIDConditionType,
			expectedConditionStatus:  "True",
			expectedConditionReason:  "{\"140\":true}",
			expectedConditionMessage: "123456",
		},
		{
			desc:                     "repetitive XID",
			events:                   []uint64{72, 140, 123},
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
		updatedNode, err := fakeClient.CoreV1().Nodes().Get(context.Background(), "test-node", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Failed to get updated node for %q: %v", test.desc, err)
		}
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

func TestRecordXIDEvent(t *testing.T) {
	node := makeNode(nil, nil, nil)
	fakeClient := fake.NewSimpleClientset(&v1.NodeList{Items: []v1.Node{node}})
	fakeRecorder := record.NewFakeRecorder(10)

	gp := mockGPUDevice{}
	device1 := pluginapi.Device{ID: "nvidia0"}
	device2 := pluginapi.Device{ID: "nvidia1"}

	tests := []struct {
		name          string
		event         nvmlEvent
		devices       map[string]pluginapi.Device
		nvmlDevices   map[string]nvmlDevice
		expectedEvent string
	}{
		{
			name: "event with no UUID",
			event: nvmlEvent{
				EventData:  72,
				DeviceUUID: "",
			},
			expectedEvent: "Warning XIDError Caught XID error, XID=72",
		},
		{
			name: "event with UUID matching a device",
			event: nvmlEvent{
				EventData:         72,
				DeviceUUID:        "GPU-1",
				GpuInstanceID:     1,
				ComputeInstanceID: 2,
			},
			devices: map[string]pluginapi.Device{
				"nvidia0": device1,
				"nvidia1": device2,
			},
			nvmlDevices: map[string]nvmlDevice{
				"nvidia0": &mockDevice{uuid: "GPU-1"},
				"nvidia1": &mockDevice{uuid: "GPU-2"},
			},
			expectedEvent: "Warning XIDError Caught XID error, XID=72, GPU UUID=GPU-1, Device ID=nvidia0",
		},
		{
			name: "event with UUID matching no devices",
			event: nvmlEvent{
				EventData:         72,
				DeviceUUID:        "GPU-3",
				GpuInstanceID:     1,
				ComputeInstanceID: 2,
			},
			devices: map[string]pluginapi.Device{
				"nvidia0": device1,
			},
			nvmlDevices: map[string]nvmlDevice{
				"nvidia0": &mockDevice{uuid: "GPU-1"},
			},
			expectedEvent: "Warning XIDError Caught XID error, XID=72, GPU UUID=GPU-3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hc := &GPUHealthChecker{
				devices:     tt.devices,
				nvmlDevices: tt.nvmlDevices,
				kubeClient:  fakeClient,
				nodeName:    "test-node",
				recorder:    fakeRecorder,
			}
			err := hc.recordXIDEvent(tt.event, &gp)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			select {
			case gotEvent := <-fakeRecorder.Events:
				if gotEvent != tt.expectedEvent {
					t.Errorf("expected event %q, got %q", tt.expectedEvent, gotEvent)
				}
			case <-time.After(100 * time.Millisecond):
				t.Errorf("expected event %q but none was recorded", tt.expectedEvent)
			}
		})
	}
}

func TestStopIdempotentAndStartStopRestart(t *testing.T) {
	node := makeNode(nil, nil, nil)
	fakeClient := fake.NewSimpleClientset(&v1.NodeList{Items: []v1.Node{node}})

	eventCreatedCh := make(chan *v1.Event, 10)
	fakeClient.Fake.PrependReactor("create", "events", func(action k8sclienttesting.Action) (bool, runtime.Object, error) {
		createAction, ok := action.(k8sclienttesting.CreateAction)
		if ok {
			if ev, ok := createAction.GetObject().(*v1.Event); ok {
				eventCreatedCh <- ev
				return true, ev, nil
			}
		}
		return false, nil, nil
	})

	devices := map[string]pluginapi.Device{
		"nvidia0": {ID: "nvidia0", Health: pluginapi.Healthy},
	}
	healthCh := make(chan pluginapi.Device, 4)

	hc := NewGPUHealthChecker(devices, healthCh, []int{79}, fakeClient)
	defer hc.Stop()
	hc.nodeName = "test-node"
	hc.eventWaitTimeoutMs = 5

	// Calling Stop before Start must be a safe no-op.
	hc.Stop()
	hc.Stop()

	gpu0Run1 := &mockDevice{
		uuid:       "GPU-0000",
		minor:      0,
		migModeRet: nvml.ERROR_NOT_SUPPORTED,
	}
	fakeNVML := &mockNVML{
		devices: []nvmlDevice{gpu0Run1},
	}
	hc.nvml = fakeNVML

	// 1. First Start.
	if err := hc.Start(); err != nil {
		t.Fatalf("first Start() failed: %v", err)
	}
	// Starting again while already running must return an error and not leak an eventSet.
	if err := hc.Start(); err == nil {
		t.Fatalf("expected error on duplicate Start(), got nil")
	}
	if len(fakeNVML.eventSets) != 1 {
		t.Fatalf("expected 1 event set created, got %d", len(fakeNVML.eventSets))
	}

	// Send an XID event during run 1.
	fakeNVML.eventSets[0].events <- nvmlEvent{
		EventType:         nvml.EventTypeXidCriticalError,
		EventData:         79,
		DeviceUUID:        "GPU-0000",
		GpuInstanceID:     noInstanceID,
		ComputeInstanceID: noInstanceID,
	}

	select {
	case unhealthy := <-healthCh:
		if unhealthy.ID != "nvidia0" || unhealthy.Health != pluginapi.Unhealthy {
			t.Fatalf("unexpected health update on run 1: %+v", unhealthy)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for health update on run 1")
	}

	select {
	case ev := <-eventCreatedCh:
		if !strings.Contains(ev.Message, "XID=79") || !strings.Contains(ev.Message, "Device ID=nvidia0") {
			t.Fatalf("unexpected k8s event message on run 1: %q", ev.Message)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for k8s event from broadcaster on run 1")
	}

	// 2. Concurrent and repeated Stop calls must be idempotent and free the event set once.
	var stopWG sync.WaitGroup
	for i := 0; i < 4; i++ {
		stopWG.Add(1)
		go func() {
			defer stopWG.Done()
			hc.Stop()
		}()
	}
	stopWG.Wait()

	if got := atomic.LoadInt32(&fakeNVML.eventSets[0].freeCount); got != 1 {
		t.Fatalf("expected run 1 eventSet.Free() called once, got %d", got)
	}
	if len(hc.nvmlDevices) != 0 {
		t.Fatalf("expected nvmlDevices cleared after Stop(), got %d entries", len(hc.nvmlDevices))
	}

	// 3. Simulate GPU reset recovery: replace NVML device handle and Start() again on same hc.
	gpu0Run2 := &mockDevice{
		uuid:       "GPU-0000",
		minor:      0,
		migModeRet: nvml.ERROR_NOT_SUPPORTED,
	}
	fakeNVML.devices = []nvmlDevice{gpu0Run2}

	if err := hc.Start(); err != nil {
		t.Fatalf("second Start() after Stop() failed: %v", err)
	}
	if len(fakeNVML.eventSets) != 2 {
		t.Fatalf("expected 2nd event set created on restart, got %d", len(fakeNVML.eventSets))
	}

	fakeNVML.eventSets[1].events <- nvmlEvent{
		EventType:         nvml.EventTypeXidCriticalError,
		EventData:         48,
		DeviceUUID:        "GPU-0000",
		GpuInstanceID:     noInstanceID,
		ComputeInstanceID: noInstanceID,
	}

	select {
	case unhealthy := <-healthCh:
		if unhealthy.ID != "nvidia0" || unhealthy.Health != pluginapi.Unhealthy {
			t.Fatalf("unexpected health update on run 2: %+v", unhealthy)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for health update on run 2")
	}

	// Verify the long-lived broadcaster still records events after Stop/Start cycle.
	select {
	case ev := <-eventCreatedCh:
		if !strings.Contains(ev.Message, "XID=48") || !strings.Contains(ev.Message, "Device ID=nvidia0") {
			t.Fatalf("unexpected k8s event message on run 2: %q", ev.Message)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for k8s event from broadcaster on run 2")
	}

	hc.Stop()
	if got := atomic.LoadInt32(&fakeNVML.eventSets[1].freeCount); got != 1 {
		t.Fatalf("expected run 2 eventSet.Free() called once, got %d", got)
	}
}

func TestStartErrorFreesEventSet(t *testing.T) {
	node := makeNode(nil, nil, nil)
	fakeClient := fake.NewSimpleClientset(&v1.NodeList{Items: []v1.Node{node}})

	devices := map[string]pluginapi.Device{
		"nvidia0": {ID: "nvidia0", Health: pluginapi.Healthy},
	}
	hc := NewGPUHealthChecker(devices, make(chan pluginapi.Device, 1), nil, fakeClient)
	hc.nodeName = "test-node"

	failingGPU := &mockDevice{
		uuid:        "GPU-fail",
		minor:       0,
		migModeRet:  nvml.ERROR_NOT_SUPPORTED,
		registerRet: nvml.ERROR_UNKNOWN,
	}
	fakeNVML := &mockNVML{
		devices: []nvmlDevice{failingGPU},
	}
	hc.nvml = fakeNVML

	if err := hc.Start(); err == nil {
		t.Fatalf("expected Start() to fail when RegisterEvents returns ERROR_UNKNOWN")
	}
	if len(fakeNVML.eventSets) != 1 {
		t.Fatalf("expected 1 eventSet created, got %d", len(fakeNVML.eventSets))
	}
	if got := atomic.LoadInt32(&fakeNVML.eventSets[0].freeCount); got != 1 {
		t.Fatalf("expected eventSet to be freed when Start() fails, got freeCount=%d", got)
	}
	if hc.running {
		t.Fatalf("expected hc.running to be false after failed Start()")
	}
}

func TestMIGSupportModernAndLegacyUUIDs(t *testing.T) {
	node := makeNode(nil, nil, nil)
	fakeClient := fake.NewSimpleClientset(&v1.NodeList{Items: []v1.Node{node}})

	devices := map[string]pluginapi.Device{
		"nvidia0/gi1": {ID: "nvidia0/gi1", Health: pluginapi.Healthy},
		"nvidia0/gi2": {ID: "nvidia0/gi2", Health: pluginapi.Healthy},
		"nvidia1/gi3": {ID: "nvidia1/gi3", Health: pluginapi.Healthy},
	}
	healthCh := make(chan pluginapi.Device, 4)

	hc := NewGPUHealthChecker(devices, healthCh, []int{79}, fakeClient)
	defer hc.Stop()
	hc.nodeName = "test-node"
	hc.eventWaitTimeoutMs = 5
	fakeRecorder := record.NewFakeRecorder(10)
	hc.recorder = fakeRecorder

	// Physical GPU 0 with modern R470+ MIG UUIDs (MIG-<uuid>, no parent GPU or GI/CI in UUID string).
	physGPU0 := &mockDevice{
		uuid:           "GPU-parent-0",
		minor:          0,
		migCurrentMode: nvml.DEVICE_MIG_ENABLE,
		migPendingMode: nvml.DEVICE_MIG_ENABLE,
		maxMigs:        4,
	}
	mig0GI1 := &mockDevice{
		uuid:   "MIG-11111111-2222-3333-4444-555555555551",
		parent: physGPU0,
		gi:     1,
		ci:     0,
	}
	mig0GI2 := &mockDevice{
		uuid:   "MIG-11111111-2222-3333-4444-555555555552",
		parent: physGPU0,
		gi:     2,
		ci:     0,
	}
	physGPU0.migByIndex = map[int]nvmlDevice{
		0: mig0GI1,
		2: mig0GI2, // sparse index (index 1 returns ERROR_NOT_FOUND)
	}

	// Physical GPU 1 with legacy R450 MIG UUID format (MIG-GPU-<uuid>/<gi>/<ci>).
	physGPU1 := &mockDevice{
		uuid:           "GPU-parent-1",
		minor:          1,
		migCurrentMode: nvml.DEVICE_MIG_ENABLE,
		migPendingMode: nvml.DEVICE_MIG_ENABLE,
		maxMigs:        2,
	}
	mig1GI3 := &mockDevice{
		uuid: "MIG-GPU-parent-1/3/0",
	}
	physGPU1.migByIndex = map[int]nvmlDevice{
		0: mig1GI3,
	}

	fakeNVML := &mockNVML{
		devices: []nvmlDevice{physGPU0, physGPU1},
		byUUID: map[string]nvmlDevice{
			mig0GI1.uuid: mig0GI1,
			mig0GI2.uuid: mig0GI2,
		},
	}
	hc.nvml = fakeNVML

	if err := hc.Start(); err != nil {
		t.Fatalf("Start() with MIG devices failed: %v", err)
	}
	defer hc.Stop()

	// Emit an XID 79 event on physical GPU 0 targeting GI=2, CI=0 (modern MIG UUID).
	fakeNVML.eventSets[0].events <- nvmlEvent{
		EventType:         nvml.EventTypeXidCriticalError,
		EventData:         79,
		DeviceUUID:        "GPU-parent-0",
		GpuInstanceID:     2,
		ComputeInstanceID: 0,
	}

	select {
	case unhealthy := <-healthCh:
		if unhealthy.ID != "nvidia0/gi2" || unhealthy.Health != pluginapi.Unhealthy {
			t.Fatalf("expected nvidia0/gi2 unhealthy, got %+v", unhealthy)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for MIG nvidia0/gi2 health update")
	}

	select {
	case ev := <-fakeRecorder.Events:
		want := "Warning XIDError Caught XID error, XID=79, GPU UUID=" + mig0GI2.uuid + ", Device ID=nvidia0/gi2"
		if ev != want {
			t.Fatalf("unexpected event for modern MIG device: got %q, want %q", ev, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for modern MIG K8s event")
	}

	// Emit an XID 79 event on physical GPU 1 targeting GI=3, CI=0 (legacy MIG UUID).
	fakeNVML.eventSets[0].events <- nvmlEvent{
		EventType:         nvml.EventTypeXidCriticalError,
		EventData:         79,
		DeviceUUID:        "GPU-parent-1",
		GpuInstanceID:     3,
		ComputeInstanceID: 0,
	}

	select {
	case unhealthy := <-healthCh:
		if unhealthy.ID != "nvidia1/gi3" || unhealthy.Health != pluginapi.Unhealthy {
			t.Fatalf("expected nvidia1/gi3 unhealthy, got %+v", unhealthy)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for legacy MIG nvidia1/gi3 health update")
	}

	select {
	case ev := <-fakeRecorder.Events:
		want := "Warning XIDError Caught XID error, XID=79, GPU UUID=" + mig1GI3.uuid + ", Device ID=nvidia1/gi3"
		if ev != want {
			t.Fatalf("unexpected event for legacy MIG device: got %q, want %q", ev, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for legacy MIG K8s event")
	}
}
