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
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/compute/metadata"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/golang/glog"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	clientv1 "k8s.io/client-go/kubernetes/typed/core/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	client "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"

	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

const (
	XIDConditionType = "XidCriticalError"
	eventSource      = "nvidia-gpu-device-plugin"

	resetXIDConditionTimeout = 2 * time.Minute
)

// GPUHealthChecker checks the health of nvidia GPUs. Note that with the current
// device naming pattern in device manager, GPUHealthChecker will not work with
// MIG devices.
type nvmlDevice interface {
	GetUUID() (string, nvml.Return)
	GetMinorNumber() (int, nvml.Return)
	GetMaxMigDeviceCount() (int, nvml.Return)
	GetMigDeviceHandleByIndex(int) (nvmlDevice, nvml.Return)
	RegisterEvents(uint64, nvml.EventSet) nvml.Return
}

type realDevice struct {
	nvml.Device
}

func (d realDevice) GetUUID() (string, nvml.Return) {
	return d.Device.GetUUID()
}

func (d realDevice) GetMinorNumber() (int, nvml.Return) {
	return d.Device.GetMinorNumber()
}

func (d realDevice) GetMaxMigDeviceCount() (int, nvml.Return) {
	return d.Device.GetMaxMigDeviceCount()
}

func (d realDevice) GetMigDeviceHandleByIndex(index int) (nvmlDevice, nvml.Return) {
	mig, ret := d.Device.GetMigDeviceHandleByIndex(index)
	if ret != nvml.SUCCESS {
		return nil, ret
	}
	return realDevice{mig}, nvml.SUCCESS
}

func (d realDevice) RegisterEvents(eventTypes uint64, set nvml.EventSet) nvml.Return {
	return d.Device.RegisterEvents(eventTypes, set)
}

type GPUHealthChecker struct {
	devices           map[string]pluginapi.Device
	nvmlDevices       map[string]nvmlDevice
	health            chan pluginapi.Device
	eventSet          nvml.EventSet
	stop              chan bool
	healthCriticalXid map[uint64]bool
	// This map is used for conditions setting and monitoring reason, will not trigger auto-repair
	monitorCriticalXid map[uint64]bool
	kubeClient         client.Interface
	nodeName           string
	recorder           record.EventRecorder
}

// NewGPUHealthChecker returns a GPUHealthChecker object for a given device name
func NewGPUHealthChecker(devices map[string]pluginapi.Device, health chan pluginapi.Device, codes []int, kubeClient client.Interface) *GPUHealthChecker {
	hc := &GPUHealthChecker{
		devices:            make(map[string]pluginapi.Device),
		nvmlDevices:        make(map[string]nvmlDevice),
		health:             health,
		stop:               make(chan bool),
		healthCriticalXid:  make(map[uint64]bool),
		monitorCriticalXid: make(map[uint64]bool),
	}
	hc.kubeClient = kubeClient

	// Create an event broadcaster
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	eventBroadcaster.StartRecordingToSink(&clientv1.EventSinkImpl{Interface: hc.kubeClient.CoreV1().Events("")})
	hc.recorder = eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: eventSource})

	// Cloning the device map to avoid interfering with the device manager
	for id, d := range devices {
		hc.devices[id] = d
	}
	for _, c := range codes {
		glog.Infof("reading code %v", c)
		hc.healthCriticalXid[uint64(c)] = true
	}

	monitorCriticalXid := []int{48, 63, 64, 79, 119, 120, 123, 140}
	for _, xid := range monitorCriticalXid {
		hc.monitorCriticalXid[uint64(xid)] = true
	}

	// By default, we check Double Bit ECC Error
	hc.healthCriticalXid[48] = true
	return hc
}

// resetXIDConditionWithBackoff tries to reset XID condition with exponential backoff.
// It retries with a 1s sleep and backs off to 30s. It times out after 2 minutes.
func (hc *GPUHealthChecker) resetXIDConditionWithBackoff() {
	backoff := 1 * time.Second
	timeout := time.After(resetXIDConditionTimeout)
	for {
		select {
		case <-timeout:
			glog.Errorf("Timeout resetting XID condition after 2 minutes.")
			return
		default:
			err := hc.resetXIDCondition()
			if err == nil {
				return
			}
			glog.Errorf("Failed to reset XID condition, will retry in %v. Error: %v", backoff, err)
			time.Sleep(backoff)
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

// Check whether the XID condition should be removed. If the conditions exists,
// 1. If the bootId changes, consider the node fixed through auto-repair
// 2. If the bootId stay unchanged, consider a pure gpu-device-plugin restart
func (hc *GPUHealthChecker) resetXIDCondition() error {
	node, err := hc.kubeClient.CoreV1().Nodes().Get(context.Background(), hc.nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	bootId := node.Status.NodeInfo.BootID
	lastBootId := ""
	newConditions := []v1.NodeCondition{}
	for _, condition := range node.Status.Conditions {
		if condition.Type == XIDConditionType && condition.Status == "True" {
			lastBootId = condition.Message
			if bootId != "" && lastBootId != "" && bootId != lastBootId {
				continue
			}
		}
		newConditions = append(newConditions, condition)
	}
	// Remove condition
	if len(newConditions) != len(node.Status.Conditions) {
		node.Status.Conditions = newConditions
		// TODO: Use patch to avoid possible conflicts?
		_, err := hc.kubeClient.CoreV1().Nodes().UpdateStatus(context.Background(), node, metav1.UpdateOptions{})
		if err != nil {
			glog.Errorf("Failed to update node %s status after removing XID condition: %v", hc.nodeName, err)
			return err
		}
		glog.Infof("Successfully removed XIDCriticalError condition from node %s.", hc.nodeName)
	} else {
		glog.Infof("XIDCriticalError condition doesn't exist for node %s.", hc.nodeName)
	}
	return nil
}

// Start registers NVML events and starts listening to them
func (hc *GPUHealthChecker) Start() error {
	nodeName, err := metadata.InstanceNameWithContext(context.Background())
	if err != nil {
		glog.Errorf("failed to get nodeName, err: %v", err)
	}
	hc.nodeName = nodeName

	go hc.resetXIDConditionWithBackoff()

	go hc.setXIDheartbeat()

	glog.Info("Starting GPU Health Checker")

	for name, device := range hc.devices {
		glog.Infof("Healthchecker receives device %s, device %v+", name, device)
	}

	// Building mapping between device ID and their nvml represetation
	count, ret := nvml.DeviceGetCount()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("failed to get device count: %v", nvml.ErrorString(ret))
	}

	glog.Infof("Found %d GPU devices", count)
	physicalDevices := make(map[string]nvmlDevice)

	for i := 0; i < count; i++ {
		device, ret := nvml.DeviceGetHandleByIndex(i)
		if ret != nvml.SUCCESS {
			return fmt.Errorf("failed to read device with index %d: %v", i, nvml.ErrorString(ret))
		}

		uuid, ret := device.GetUUID()
		if ret != nvml.SUCCESS {
			glog.Errorf("Failed to get UUID for device %d: %v. Skipping", i, nvml.ErrorString(ret))
			continue
		}
		physicalDevices[uuid] = realDevice{device}

		minor, ret := device.GetMinorNumber()
		if ret != nvml.SUCCESS {
			glog.Errorf("Failed to get minor number for device %d: %v. Skipping this device", i, nvml.ErrorString(ret))
			continue
		}
		deviceName := fmt.Sprintf("nvidia%d", minor)

		currentMode, _, ret := device.GetMigMode()
		if ret != nvml.SUCCESS {
			glog.Errorf("Error checking MIG mode on device %s. Skipping this device. Error: %v", deviceName, nvml.ErrorString(ret))
			continue
		}
		migEnabled := (currentMode == nvml.DEVICE_MIG_ENABLE)

		if migEnabled {
			if err := hc.addMigEnabledDevice(deviceName, realDevice{device}); err != nil {
				glog.Errorf("Failed to add MIG-enabled device %s for health check. Skipping this device. Error: %v", deviceName, err)
				continue
			}
		} else {
			hc.addDevice(deviceName, realDevice{device})
		}
	}

	hc.eventSet, ret = nvml.EventSetCreate()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("failed to create event set: %v", nvml.ErrorString(ret))
	}
	for name, d := range hc.nvmlDevices {
		uuid, ret := d.GetUUID()
		if ret != nvml.SUCCESS {
			return fmt.Errorf("failed to get UUID for device %s: %v", name, nvml.ErrorString(ret))
		}
		var gpu string
		var parseErr error
		gpu, _, _, parseErr = parseMigDeviceUUIDHelper(uuid)
		if parseErr != nil {
			gpu = uuid
		}

		physDevice, ok := physicalDevices[gpu]
		if !ok {
			return fmt.Errorf("physical GPU %s not found for device %s", gpu, name)
		}

		glog.Infof("Registering device %s (physical GPU %s). UUID: %s", name, gpu, uuid)
		ret = physDevice.RegisterEvents(nvml.EventTypeXidCriticalError, hc.eventSet)
		if ret != nvml.SUCCESS {
			if ret == nvml.ERROR_NOT_SUPPORTED {
				glog.Warningf("Warning: %s does not support healthchecking: %v. It will always be marked healthy.", name, nvml.ErrorString(ret))
				continue
			} else {
				return fmt.Errorf("failed to register device %s for NVML eventSet: %v", name, nvml.ErrorString(ret))
			}
		}
	}

	go func() {
		if err := hc.listenToEvents(); err != nil {
			glog.Errorf("GPUHealthChecker listenToEvents error: %v", err)
		}
	}()

	return nil
}

func (hc *GPUHealthChecker) addDevice(deviceName string, device nvmlDevice) {
	if _, ok := hc.devices[deviceName]; !ok {
		// Only monitor the devices passed in
		glog.Warningf("Ignoring device %s for health check.", deviceName)
		return
	}
	uuid, ret := device.GetUUID()
	if ret != nvml.SUCCESS {
		glog.Errorf("Failed to get UUID for device %s: %v. Skipping health monitoring.", deviceName, nvml.ErrorString(ret))
		return
	}
	glog.Infof("Found non-mig device %s for health monitoring. UUID: %s", deviceName, uuid)
	hc.nvmlDevices[deviceName] = device
}

func (hc *GPUHealthChecker) getMigDevices(device nvmlDevice) ([]nvmlDevice, error) {
	maxMigs, ret := device.GetMaxMigDeviceCount()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("failed to get max MIG devices: %v", nvml.ErrorString(ret))
	}
	var migs []nvmlDevice
	for i := 0; i < maxMigs; i++ {
		migDevice, ret := device.GetMigDeviceHandleByIndex(i)
		if ret == nvml.ERROR_NOT_FOUND {
			continue // No MIG device at this index
		}
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("failed to get MIG device at index %d: %v", i, nvml.ErrorString(ret))
		}
		migs = append(migs, migDevice)
	}
	return migs, nil
}

func (hc *GPUHealthChecker) addMigEnabledDevice(deviceName string, device nvmlDevice) error {
	glog.Infof("HealthChecker detects MIG is enabled on device %s", deviceName)

	migs, err := hc.getMigDevices(device)
	if err != nil {
		return fmt.Errorf("error getting MIG devices on device %s. err: %v.", deviceName, err)
	}

	for _, mig := range migs {
		uuid, ret := mig.GetUUID()
		if ret != nvml.SUCCESS {
			return fmt.Errorf("failed to get MIG UUID: %v", nvml.ErrorString(ret))
		}
		gpu, gi, _, err := parseMigDeviceUUIDHelper(uuid)
		if err != nil {
			return fmt.Errorf("error parsing MIG UUID on device %s, MIG UUID: %s, error %v", gpu, uuid, err)
		}
		migDeviceName := fmt.Sprintf("%s/gi%d", deviceName, gi)

		if _, ok := hc.devices[migDeviceName]; !ok {
			// Only monitor the devices passed in
			glog.Warningf("Ignoring device %s for health check.", migDeviceName)
			continue
		}
		glog.Infof("Found mig device %s for health monitoring. UUID: %s", migDeviceName, uuid)
		hc.nvmlDevices[migDeviceName] = mig
	}
	return nil
}

type callDevice interface {
	parseMigDeviceUUID(UUID string) (string, uint, uint, error)
}
type GPUDevice struct{}

var migUUIDRegex = regexp.MustCompile(`^MIG-GPU-([^/]+)/([0-9]+)/([0-9]+)$`)

func parseMigDeviceUUIDHelper(UUID string) (string, uint, uint, error) {
	matches := migUUIDRegex.FindStringSubmatch(UUID)
	if len(matches) != 4 {
		return "", 0, 0, fmt.Errorf("invalid MIG device UUID format: %s", UUID)
	}
	gpuUUID := "GPU-" + matches[1]
	gi, err := strconv.ParseUint(matches[2], 10, 32)
	if err != nil {
		return "", 0, 0, fmt.Errorf("invalid GPU instance ID: %v", err)
	}
	ci, err := strconv.ParseUint(matches[3], 10, 32)
	if err != nil {
		return "", 0, 0, fmt.Errorf("invalid compute instance ID: %v", err)
	}
	return gpuUUID, uint(gi), uint(ci), nil
}

func (gd *GPUDevice) parseMigDeviceUUID(UUID string) (string, uint, uint, error) {
	return parseMigDeviceUUIDHelper(UUID)
}

func (hc *GPUHealthChecker) monitorXidevent(eventData uint64) {
	if _, ok := hc.monitorCriticalXid[eventData]; ok {
		glog.Info("Monitoring XID event")
		// Set XID condition
		node, err := hc.kubeClient.CoreV1().Nodes().Get(context.Background(), hc.nodeName, metav1.GetOptions{})
		if err != nil {
			glog.Errorf("Failed to get node %s: %v", hc.nodeName, err)
			return
		}
		conditionFound := false
		for i := range node.Status.Conditions {
			condition := &node.Status.Conditions[i]
			if condition.Type == XIDConditionType {
				conditionFound = true
				var genericMap map[string]interface{}
				err := json.Unmarshal([]byte(condition.Reason), &genericMap)
				if err != nil {
					glog.Errorf("Can't decode the value of condition.Reason %s", condition.Reason)
					return
				}
				xidStr := strconv.FormatUint(eventData, 10)
				if _, ok := genericMap[xidStr]; ok {
					glog.Infof("XIDCritialError Condition already includes this XID %v, skip", eventData)
					return
				}
				genericMap[xidStr] = true
				jsonStr, err := json.Marshal(genericMap)
				if err != nil {
					glog.Errorf("Can't encode the value of condition.Reason %s", condition.Reason)
					return
				}
				condition.Reason = string(jsonStr)
			}
		}
		if !conditionFound {
			glog.Infof("XIDCritialError Condition not exists, adding:", eventData)
			genericMap := map[string]interface{}{strconv.FormatUint(eventData, 10): true}
			jsonStr, err := json.Marshal(genericMap)
			if err != nil {
				glog.Errorf("Can't encode the value of genericMap: %s", genericMap)
				return
			}
			node.Status.Conditions = append(node.Status.Conditions, v1.NodeCondition{
				Type:               XIDConditionType,
				Status:             "True",
				LastHeartbeatTime:  metav1.Now(),
				LastTransitionTime: metav1.Now(),
				Reason:             string(jsonStr),
				Message:            node.Status.NodeInfo.BootID,
			})
		}
		_, err = hc.kubeClient.CoreV1().Nodes().UpdateStatus(context.Background(), node, metav1.UpdateOptions{})
		if err != nil {
			glog.Errorf("Failed to update node %s status to add XIDCriticalError condition: %v", hc.nodeName, err)
		} else {
			glog.Infof("Successfully add XIDCriticalError condition from node %s.", hc.nodeName)
		}
	}
}

func (hc *GPUHealthChecker) setXIDheartbeat() {
	for {
		select {
		case <-hc.stop:
			return
		default:
			hc.updateLastHeartbeatTime()
			time.Sleep(1 * time.Minute)
		}
	}
}

func (hc *GPUHealthChecker) updateLastHeartbeatTime() {
	node, err := hc.kubeClient.CoreV1().Nodes().Get(context.Background(), hc.nodeName, metav1.GetOptions{})
	if err != nil {
		glog.Errorf("Failed to get node %s for heartbeat update: %v", hc.nodeName, err)
		return
	}

	nodeModified := false
	for i := range node.Status.Conditions {
		if node.Status.Conditions[i].Type == XIDConditionType && node.Status.Conditions[i].Status == "True" {
			node.Status.Conditions[i].LastHeartbeatTime = metav1.Now()
			nodeModified = true
		}
	}

	if !nodeModified {
		return
	}

	glog.Info("XID heartbeat check")
	_, err = hc.kubeClient.CoreV1().Nodes().UpdateStatus(context.Background(), node, metav1.UpdateOptions{})
	if err != nil {
		glog.Errorf("Failed to update node %s status to update XIDCondition heartbeat: %v", hc.nodeName, err)
	}
}

func (hc *GPUHealthChecker) recordXIDEvent(eventData uint64, deviceUUID string, gpuInstanceID uint32, computeInstanceID uint32, cd callDevice) error {
	node, err := hc.kubeClient.CoreV1().Nodes().Get(context.Background(), hc.nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Caught XID error, XID=%d", eventData)

	if deviceUUID != "" {
		var affectedGpuIDs []string
		var affectedGpuUUIDs []string

		for _, d := range hc.devices {
			nvmlDev, ok := hc.nvmlDevices[d.ID]
			if !ok || nvmlDev == nil {
				continue
			}
			uuid, ret := nvmlDev.GetUUID()
			if ret != nvml.SUCCESS {
				glog.Errorf("Failed to get UUID for device %s: %v", d.ID, nvml.ErrorString(ret))
				continue
			}
			gpu, gi, ci, err := cd.parseMigDeviceUUID(uuid)
			if err != nil {
				gpu = uuid
				gi = 0xFFFFFFFF
				ci = 0xFFFFFFFF
			}

			if gpu == deviceUUID && uint32(gi) == gpuInstanceID && uint32(ci) == computeInstanceID {
				affectedGpuIDs = append(affectedGpuIDs, d.ID)
				affectedGpuUUIDs = append(affectedGpuUUIDs, uuid)
			}
		}

		if len(affectedGpuIDs) > 0 {
			fmt.Fprintf(&sb, ", GPU UUID=%s", strings.Join(affectedGpuUUIDs, ", "))
			fmt.Fprintf(&sb, ", Device ID=%s", strings.Join(affectedGpuIDs, ", "))
		} else {
			fmt.Fprintf(&sb, ", GPU UUID=%s", deviceUUID)
		}
	}

	msg := sb.String()
	hc.recorder.Eventf(node, v1.EventTypeWarning, "XIDError", msg)
	return nil
}

func (hc *GPUHealthChecker) catchError(eventType uint64, eventData uint64, deviceUUID string, gpuInstanceID uint32, computeInstanceID uint32, cd callDevice) {
	// Skip the error if it's not Xid critical
	if eventType != nvml.EventTypeXidCriticalError {
		glog.Infof("Skip error Xid=%d as it is not Xid Critical", eventData)
		return
	}

	err := hc.recordXIDEvent(eventData, deviceUUID, gpuInstanceID, computeInstanceID, cd)
	if err != nil {
		glog.Errorf("Failed to record XID=%d for node %s with err %v", eventData, hc.nodeName, err)
	}
	hc.monitorXidevent(eventData)

	// Only marking device unhealthy on Double Bit ECC Error or customer-configured codes
	// See https://docs.nvidia.com/deploy/xid-errors/index.html#topic_4
	if _, ok := hc.healthCriticalXid[eventData]; !ok {
		glog.Infof("Health checker is skipping Xid %v error", eventData)
		return
	}

	if deviceUUID == "" {
		// All devices are unhealthy
		glog.Errorf("XidCriticalError: Xid=%d, All devices will go unhealthy.", eventData)
		for id, d := range hc.devices {
			d.Health = pluginapi.Unhealthy
			hc.devices[id] = d
			hc.health <- d
		}
		return
	}

	founderrordevice := false
	for _, d := range hc.devices {
		nvmlDev, ok := hc.nvmlDevices[d.ID]
		if !ok || nvmlDev == nil {
			continue
		}
		uuid, ret := nvmlDev.GetUUID()
		if ret != nvml.SUCCESS {
			glog.Errorf("Failed to get UUID for device %s: %v", d.ID, nvml.ErrorString(ret))
			continue
		}
		gpu, gi, ci, err := cd.parseMigDeviceUUID(uuid)
		if err != nil {
			gpu = uuid
			gi = 0xFFFFFFFF
			ci = 0xFFFFFFFF
		}

		if gpu == deviceUUID && uint32(gi) == gpuInstanceID && uint32(ci) == computeInstanceID {
			glog.Errorf("XidCriticalError: Xid=%d on Device=%s, uuid=%s, the device will go unhealthy.", eventData, d.ID, uuid)
			d.Health = pluginapi.Unhealthy
			hc.devices[d.ID] = d
			hc.health <- d
			founderrordevice = true
		}
	}
	if !founderrordevice {
		glog.Errorf("XidCriticalError: Xid=%d on unknown device.", eventData)
	}
}

// listenToEvents listens to events from NVML to detect GPU critical errors
func (hc *GPUHealthChecker) listenToEvents() error {
	for {
		select {
		case <-hc.stop:
			close(hc.stop)
			return nil
		default:
		}

		e, ret := nvml.EventSetWait(hc.eventSet, 5000)
		if ret != nvml.SUCCESS {
			if ret == nvml.ERROR_TIMEOUT {
				continue
			}
			glog.Errorf("EventSetWait failed: %v", nvml.ErrorString(ret))
			time.Sleep(1 * time.Second) // Avoid tight loop on persistent error
			continue
		}

		var deviceUUID string
		if e.Device.Handle != nil {
			var ret nvml.Return
			deviceUUID, ret = e.Device.GetUUID()
			if ret != nvml.SUCCESS {
				glog.Errorf("Failed to get UUID from event device: %v", nvml.ErrorString(ret))
			}
		}

		gd := GPUDevice{}
		hc.catchError(e.EventType, e.EventData, deviceUUID, e.GpuInstanceId, e.ComputeInstanceId, &gd)
	}
}

// Stop deletes the NVML events and stops the listening go routine
func (hc *GPUHealthChecker) Stop() {
	hc.recorder.(record.EventBroadcaster).Shutdown()
	hc.eventSet.Free()
	hc.stop <- true
	<-hc.stop
}
