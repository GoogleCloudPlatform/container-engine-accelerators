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
	"sort"
	"strconv"
	"strings"
	"sync"
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

	resetXIDConditionTimeout  = 2 * time.Minute
	defaultBackoffInitial     = 1 * time.Second
	defaultBackoffMax         = 30 * time.Second
	defaultHeartbeatInterval  = 1 * time.Minute
	defaultEventWaitTimeoutMs = uint32(5000)
	defaultEventRetryDelay    = 1 * time.Second
	metadataLookupTimeout     = 5 * time.Second

	noInstanceID = uint32(0xFFFFFFFF)
)

// nvmlEvent represents an NVML event received from an event set.
type nvmlEvent struct {
	EventType         uint64
	EventData         uint64
	DeviceUUID        string
	GpuInstanceID     uint32
	ComputeInstanceID uint32
}

// nvmlEventSet abstracts an NVML event set for lifecycle management and testing.
type nvmlEventSet interface {
	Wait(timeoutMs uint32) (nvmlEvent, nvml.Return)
	Free() nvml.Return
}

// nvmlDevice abstracts an NVML device handle.
type nvmlDevice interface {
	GetUUID() (string, nvml.Return)
	GetMinorNumber() (int, nvml.Return)
	GetMigMode() (int, int, nvml.Return)
	GetMaxMigDeviceCount() (int, nvml.Return)
	GetMigDeviceHandleByIndex(int) (nvmlDevice, nvml.Return)
	GetDeviceHandleFromMigDeviceHandle() (nvmlDevice, nvml.Return)
	GetGpuInstanceId() (int, nvml.Return)
	GetComputeInstanceId() (int, nvml.Return)
	RegisterEvents(uint64, nvmlEventSet) nvml.Return
}

// nvmlProvider abstracts top-level NVML entrypoints used by GPUHealthChecker.
type nvmlProvider interface {
	DeviceGetCount() (int, nvml.Return)
	DeviceGetHandleByIndex(index int) (nvmlDevice, nvml.Return)
	DeviceGetHandleByUUID(uuid string) (nvmlDevice, nvml.Return)
	EventSetCreate() (nvmlEventSet, nvml.Return)
}

func nvmlErrorString(ret nvml.Return) string {
	if err := nvml.GetLibrary().Lookup("nvmlErrorString"); err == nil {
		return nvml.ErrorString(ret)
	}
	return fmt.Sprintf("nvml.Return(%d)", ret)
}

type realEventSet struct {
	set nvml.EventSet
}

func (es realEventSet) Wait(timeoutMs uint32) (nvmlEvent, nvml.Return) {
	e, ret := nvml.EventSetWait(es.set, timeoutMs)
	if ret != nvml.SUCCESS {
		return nvmlEvent{}, ret
	}
	var deviceUUID string
	if e.Device.Handle != nil {
		var uuidRet nvml.Return
		deviceUUID, uuidRet = e.Device.GetUUID()
		if uuidRet != nvml.SUCCESS {
			glog.Errorf("Failed to get UUID from event device: %v", nvmlErrorString(uuidRet))
		}
	}
	return nvmlEvent{
		EventType:         e.EventType,
		EventData:         e.EventData,
		DeviceUUID:        deviceUUID,
		GpuInstanceID:     e.GpuInstanceId,
		ComputeInstanceID: e.ComputeInstanceId,
	}, nvml.SUCCESS
}

func (es realEventSet) Free() nvml.Return {
	return es.set.Free()
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

func (d realDevice) GetMigMode() (int, int, nvml.Return) {
	return d.Device.GetMigMode()
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

func (d realDevice) GetDeviceHandleFromMigDeviceHandle() (nvmlDevice, nvml.Return) {
	parent, ret := d.Device.GetDeviceHandleFromMigDeviceHandle()
	if ret != nvml.SUCCESS {
		return nil, ret
	}
	return realDevice{parent}, nvml.SUCCESS
}

func (d realDevice) GetGpuInstanceId() (int, nvml.Return) {
	return d.Device.GetGpuInstanceId()
}

func (d realDevice) GetComputeInstanceId() (int, nvml.Return) {
	return d.Device.GetComputeInstanceId()
}

func (d realDevice) RegisterEvents(eventTypes uint64, set nvmlEventSet) nvml.Return {
	realSet, ok := set.(realEventSet)
	if !ok {
		return nvml.ERROR_INVALID_ARGUMENT
	}
	return d.Device.RegisterEvents(eventTypes, realSet.set)
}

type realNVML struct{}

func (realNVML) DeviceGetCount() (int, nvml.Return) {
	return nvml.DeviceGetCount()
}

func (realNVML) DeviceGetHandleByIndex(index int) (nvmlDevice, nvml.Return) {
	d, ret := nvml.DeviceGetHandleByIndex(index)
	if ret != nvml.SUCCESS {
		return nil, ret
	}
	return realDevice{d}, nvml.SUCCESS
}

func (realNVML) DeviceGetHandleByUUID(uuid string) (nvmlDevice, nvml.Return) {
	if err := nvml.GetLibrary().Lookup("nvmlDeviceGetHandleByUUID"); err != nil {
		return nil, nvml.ERROR_UNINITIALIZED
	}
	d, ret := nvml.DeviceGetHandleByUUID(uuid)
	if ret != nvml.SUCCESS {
		return nil, ret
	}
	return realDevice{d}, nvml.SUCCESS
}

func (realNVML) EventSetCreate() (nvmlEventSet, nvml.Return) {
	es, ret := nvml.EventSetCreate()
	if ret != nvml.SUCCESS {
		return nil, ret
	}
	return realEventSet{set: es}, nvml.SUCCESS
}

// GPUHealthChecker checks the health of nvidia GPUs.
type GPUHealthChecker struct {
	mu                sync.Mutex
	devMu             sync.Mutex
	nodeNameMu        sync.Mutex
	wg                sync.WaitGroup
	running           bool
	devices           map[string]pluginapi.Device
	nvmlDevices       map[string]nvmlDevice
	nvml              nvmlProvider
	health            chan pluginapi.Device
	eventSet          nvmlEventSet
	stop              chan struct{}
	healthCriticalXid map[uint64]bool
	// This map is used for conditions setting and monitoring reason, will not trigger auto-repair
	monitorCriticalXid map[uint64]bool
	kubeClient         client.Interface
	nodeName           string
	recorder           record.EventRecorder

	heartbeatInterval  time.Duration
	eventWaitTimeoutMs uint32
	eventRetryDelay    time.Duration
}

// NewGPUHealthChecker returns a GPUHealthChecker object for a given device map.
func NewGPUHealthChecker(devices map[string]pluginapi.Device, health chan pluginapi.Device, codes []int, kubeClient client.Interface) *GPUHealthChecker {
	hc := &GPUHealthChecker{
		devices:            make(map[string]pluginapi.Device),
		nvmlDevices:        make(map[string]nvmlDevice),
		nvml:               realNVML{},
		health:             health,
		stop:               make(chan struct{}),
		healthCriticalXid:  make(map[uint64]bool),
		monitorCriticalXid: make(map[uint64]bool),
		kubeClient:         kubeClient,
		heartbeatInterval:  defaultHeartbeatInterval,
		eventWaitTimeoutMs: defaultEventWaitTimeoutMs,
		eventRetryDelay:    defaultEventRetryDelay,
	}

	// Create one long-lived event broadcaster for the lifetime of the health checker.
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	if hc.kubeClient != nil {
		eventBroadcaster.StartRecordingToSink(&clientv1.EventSinkImpl{Interface: hc.kubeClient.CoreV1().Events("")})
	}
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

func (hc *GPUHealthChecker) nvmlProvider() nvmlProvider {
	if hc.nvml != nil {
		return hc.nvml
	}
	return realNVML{}
}

func (hc *GPUHealthChecker) resolveNodeName() string {
	hc.nodeNameMu.Lock()
	defer hc.nodeNameMu.Unlock()
	if hc.nodeName != "" {
		return hc.nodeName
	}
	ctx, cancel := context.WithTimeout(context.Background(), metadataLookupTimeout)
	defer cancel()
	nodeName, err := metadata.InstanceNameWithContext(ctx)
	if err != nil {
		glog.Errorf("failed to get nodeName, err: %v", err)
		return ""
	}
	hc.nodeName = nodeName
	return hc.nodeName
}

// RefreshDevices replaces the tracked device map with a clone of devices. If
// the health checker is currently running, it also re-discovers NVML handles
// so newly created MIG GPU instances (e.g. with renumbered GI IDs after a GPU
// reset) are monitored.
func (hc *GPUHealthChecker) RefreshDevices(devices map[string]pluginapi.Device) {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	hc.devMu.Lock()
	defer hc.devMu.Unlock()

	hc.devices = make(map[string]pluginapi.Device, len(devices))
	for id, d := range devices {
		hc.devices[id] = d
	}
	if hc.running {
		if _, err := hc.populateNVMLDevicesLocked(); err != nil {
			glog.Errorf("Failed to refresh NVML devices in GPUHealthChecker: %v", err)
		}
	}
}

// resetXIDConditionWithBackoff tries to reset XID condition with exponential backoff.
// It retries with a 1s sleep and backs off to 30s. It times out after 2 minutes.
func (hc *GPUHealthChecker) resetXIDConditionWithBackoff(stopCh <-chan struct{}) {
	backoff := defaultBackoffInitial
	timeoutTimer := time.NewTimer(resetXIDConditionTimeout)
	defer timeoutTimer.Stop()
	for {
		err := hc.resetXIDCondition()
		if err == nil {
			return
		}
		glog.Errorf("Failed to reset XID condition, will retry in %v. Error: %v", backoff, err)
		retryTimer := time.NewTimer(backoff)
		select {
		case <-stopCh:
			retryTimer.Stop()
			return
		case <-timeoutTimer.C:
			retryTimer.Stop()
			glog.Errorf("Timeout resetting XID condition after 2 minutes.")
			return
		case <-retryTimer.C:
		}
		backoff *= 2
		if backoff > defaultBackoffMax {
			backoff = defaultBackoffMax
		}
	}
}

// Check whether the XID condition should be removed. If the conditions exists,
// 1. If the bootId changes, consider the node fixed through auto-repair
// 2. If the bootId stay unchanged, consider a pure gpu-device-plugin restart
func (hc *GPUHealthChecker) resetXIDCondition() error {
	nodeName := hc.resolveNodeName()
	node, err := hc.kubeClient.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
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
			glog.Errorf("Failed to update node %s status after removing XID condition: %v", nodeName, err)
			return err
		}
		glog.Infof("Successfully removed XIDCriticalError condition from node %s.", nodeName)
	} else {
		glog.Infof("XIDCriticalError condition doesn't exist for node %s.", nodeName)
	}
	return nil
}

func isMigEnabled(device nvmlDevice) (bool, error) {
	currentMode, pendingMode, ret := device.GetMigMode()
	if ret == nvml.ERROR_NOT_SUPPORTED {
		return false, nil
	}
	if ret != nvml.SUCCESS {
		return false, fmt.Errorf("%s", nvmlErrorString(ret))
	}
	return currentMode == nvml.DEVICE_MIG_ENABLE && currentMode == pendingMode, nil
}

func (hc *GPUHealthChecker) populateNVMLDevicesLocked() (map[string]nvmlDevice, error) {
	for name, device := range hc.devices {
		device.Health = pluginapi.Healthy
		hc.devices[name] = device
		glog.Infof("Healthchecker receives device %s, device %v+", name, device)
	}

	provider := hc.nvmlProvider()
	hc.nvmlDevices = make(map[string]nvmlDevice)

	// Building mapping between device ID and their nvml representation
	count, ret := provider.DeviceGetCount()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("failed to get device count: %v", nvmlErrorString(ret))
	}

	glog.Infof("Found %d GPU devices", count)
	physicalDevices := make(map[string]nvmlDevice)

	for i := 0; i < count; i++ {
		device, ret := provider.DeviceGetHandleByIndex(i)
		if ret != nvml.SUCCESS {
			hc.nvmlDevices = make(map[string]nvmlDevice)
			return nil, fmt.Errorf("failed to read device with index %d: %v", i, nvmlErrorString(ret))
		}

		uuid, ret := device.GetUUID()
		if ret != nvml.SUCCESS {
			glog.Errorf("Failed to get UUID for device %d: %v. Skipping", i, nvmlErrorString(ret))
			continue
		}
		physicalDevices[uuid] = device

		minor, ret := device.GetMinorNumber()
		if ret != nvml.SUCCESS {
			glog.Errorf("Failed to get minor number for device %d: %v. Skipping this device", i, nvmlErrorString(ret))
			continue
		}
		deviceName := fmt.Sprintf("nvidia%d", minor)

		migEnabled, err := isMigEnabled(device)
		if err != nil {
			glog.Errorf("Error checking if MIG is enabled on device %s. Skipping this device. Error: %v", deviceName, err)
			continue
		}

		if migEnabled {
			if err := hc.addMigEnabledDevice(deviceName, device); err != nil {
				glog.Errorf("Failed to add MIG-enabled device %s for health check. Skipping this device. Error: %v", deviceName, err)
				continue
			}
		} else {
			hc.addDevice(deviceName, device)
		}
	}
	return physicalDevices, nil
}

// Start registers NVML events and starts listening to them.
func (hc *GPUHealthChecker) Start() error {
	hc.mu.Lock()
	defer hc.mu.Unlock()

	if hc.running {
		return fmt.Errorf("GPUHealthChecker is already running")
	}

	glog.Info("Starting GPU Health Checker")

	hc.devMu.Lock()
	physicalDevices, err := hc.populateNVMLDevicesLocked()
	if err != nil {
		hc.devMu.Unlock()
		return err
	}

	provider := hc.nvmlProvider()
	eventSet, ret := provider.EventSetCreate()
	if ret != nvml.SUCCESS {
		hc.nvmlDevices = make(map[string]nvmlDevice)
		hc.devMu.Unlock()
		return fmt.Errorf("failed to create event set: %v", nvmlErrorString(ret))
	}

	gd := GPUDevice{nvml: provider}
	registeredPhys := make(map[string]bool)
	for name, d := range hc.nvmlDevices {
		uuid, ret := d.GetUUID()
		if ret != nvml.SUCCESS {
			if freeRet := eventSet.Free(); freeRet != nvml.SUCCESS {
				glog.Errorf("Failed to free event set after error: %v", nvmlErrorString(freeRet))
			}
			hc.nvmlDevices = make(map[string]nvmlDevice)
			hc.devMu.Unlock()
			return fmt.Errorf("failed to get UUID for device %s: %v", name, nvmlErrorString(ret))
		}

		gpu, _, _, parseErr := resolveMigDeviceInfo(d, uuid, &gd)
		if parseErr != nil {
			gpu = uuid
		}

		physDevice, ok := physicalDevices[gpu]
		if !ok {
			if freeRet := eventSet.Free(); freeRet != nvml.SUCCESS {
				glog.Errorf("Failed to free event set after error: %v", nvmlErrorString(freeRet))
			}
			hc.nvmlDevices = make(map[string]nvmlDevice)
			hc.devMu.Unlock()
			return fmt.Errorf("physical GPU %s not found for device %s", gpu, name)
		}

		if registeredPhys[gpu] {
			glog.Infof("Physical GPU %s already registered for eventSet (device %s, UUID: %s)", gpu, name, uuid)
			continue
		}

		glog.Infof("Registering device %s (physical GPU %s). UUID: %s", name, gpu, uuid)
		ret = physDevice.RegisterEvents(nvml.EventTypeXidCriticalError, eventSet)
		if ret != nvml.SUCCESS {
			if ret == nvml.ERROR_NOT_SUPPORTED {
				glog.Warningf("Warning: %s does not support healthchecking: %v. It will always be marked healthy.", name, nvmlErrorString(ret))
				continue
			}
			if freeRet := eventSet.Free(); freeRet != nvml.SUCCESS {
				glog.Errorf("Failed to free event set after error: %v", nvmlErrorString(freeRet))
			}
			hc.nvmlDevices = make(map[string]nvmlDevice)
			hc.devMu.Unlock()
			return fmt.Errorf("failed to register device %s for NVML eventSet: %v", name, nvmlErrorString(ret))
		}
		registeredPhys[gpu] = true
	}
	hc.devMu.Unlock()

	stopCh := make(chan struct{})
	hc.eventSet = eventSet
	hc.stop = stopCh
	hc.running = true

	hc.wg.Add(3)
	go func() {
		defer hc.wg.Done()
		hc.resetXIDConditionWithBackoff(stopCh)
	}()
	go func() {
		defer hc.wg.Done()
		hc.setXIDheartbeat(stopCh)
	}()
	go func() {
		defer hc.wg.Done()
		if err := hc.listenToEvents(stopCh, eventSet); err != nil {
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
		glog.Errorf("Failed to get UUID for device %s: %v. Skipping health monitoring.", deviceName, nvmlErrorString(ret))
		return
	}
	glog.Infof("Found non-mig device %s for health monitoring. UUID: %s", deviceName, uuid)
	hc.nvmlDevices[deviceName] = device
}

func (hc *GPUHealthChecker) getMigDevices(device nvmlDevice) ([]nvmlDevice, error) {
	maxMigs, ret := device.GetMaxMigDeviceCount()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("failed to get max MIG devices: %v", nvmlErrorString(ret))
	}
	var migs []nvmlDevice
	for i := 0; i < maxMigs; i++ {
		migDevice, ret := device.GetMigDeviceHandleByIndex(i)
		if ret == nvml.ERROR_NOT_FOUND {
			continue // No MIG device at this index
		}
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("failed to get MIG device at index %d: %v", i, nvmlErrorString(ret))
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

	gd := GPUDevice{nvml: hc.nvmlProvider()}
	for _, mig := range migs {
		uuid, ret := mig.GetUUID()
		if ret != nvml.SUCCESS {
			return fmt.Errorf("failed to get MIG UUID: %v", nvmlErrorString(ret))
		}
		gpu, gi, _, err := resolveMigDeviceInfo(mig, uuid, &gd)
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
	parseMigDeviceUUID(UUID string) (string, uint32, uint32, error)
}

type GPUDevice struct {
	nvml nvmlProvider
}

var migUUIDRegex = regexp.MustCompile(`^MIG-GPU-([^/]+)/([0-9]+)/([0-9]+)$`)

func getMIGDeviceInfo(migDevice nvmlDevice) (string, uint32, uint32, error) {
	if migDevice == nil {
		return "", 0, 0, fmt.Errorf("nil MIG device")
	}
	parent, ret := migDevice.GetDeviceHandleFromMigDeviceHandle()
	if ret != nvml.SUCCESS || parent == nil {
		return "", 0, 0, fmt.Errorf("failed to get parent device handle from MIG device: %v", nvmlErrorString(ret))
	}
	parentUUID, ret := parent.GetUUID()
	if ret != nvml.SUCCESS {
		return "", 0, 0, fmt.Errorf("failed to get parent GPU UUID: %v", nvmlErrorString(ret))
	}
	gi, ret := migDevice.GetGpuInstanceId()
	if ret != nvml.SUCCESS || gi < 0 {
		return "", 0, 0, fmt.Errorf("failed to get GPU instance ID: %v", nvmlErrorString(ret))
	}
	ci, ret := migDevice.GetComputeInstanceId()
	if ret != nvml.SUCCESS || ci < 0 {
		return "", 0, 0, fmt.Errorf("failed to get compute instance ID: %v", nvmlErrorString(ret))
	}
	return parentUUID, uint32(gi), uint32(ci), nil
}

func resolveMigDeviceInfo(d nvmlDevice, uuid string, cd callDevice) (string, uint32, uint32, error) {
	if d != nil && strings.HasPrefix(uuid, "MIG-") {
		if gpu, gi, ci, err := getMIGDeviceInfo(d); err == nil {
			return gpu, gi, ci, nil
		}
	}
	if cd != nil {
		return cd.parseMigDeviceUUID(uuid)
	}
	return parseMigDeviceUUIDHelper(uuid)
}

func parseMigDeviceUUIDHelper(UUID string) (string, uint32, uint32, error) {
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
	return gpuUUID, uint32(gi), uint32(ci), nil
}

func (gd *GPUDevice) parseMigDeviceUUID(UUID string) (string, uint32, uint32, error) {
	if !strings.HasPrefix(UUID, "MIG-") {
		return parseMigDeviceUUIDHelper(UUID)
	}
	provider := gd.nvml
	if provider == nil {
		provider = realNVML{}
	}
	if migDev, ret := provider.DeviceGetHandleByUUID(UUID); ret == nvml.SUCCESS && migDev != nil {
		return getMIGDeviceInfo(migDev)
	}
	return parseMigDeviceUUIDHelper(UUID)
}

func (hc *GPUHealthChecker) monitorXidevent(eventData uint64) {
	if _, ok := hc.monitorCriticalXid[eventData]; ok {
		glog.Info("Monitoring XID event")
		nodeName := hc.resolveNodeName()
		// Set XID condition
		node, err := hc.kubeClient.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
		if err != nil {
			glog.Errorf("Failed to get node %s: %v", nodeName, err)
			return
		}
		conditionFound := false
		for i := range node.Status.Conditions {
			condition := &node.Status.Conditions[i]
			if condition.Type == XIDConditionType {
				conditionFound = true
				var xidMap map[string]bool
				err := json.Unmarshal([]byte(condition.Reason), &xidMap)
				if err != nil {
					glog.Errorf("Can't decode the value of condition.Reason %s", condition.Reason)
					return
				}
				xidStr := strconv.FormatUint(eventData, 10)
				if _, ok := xidMap[xidStr]; ok {
					glog.Infof("XIDCritialError Condition already includes this XID %v, skip", eventData)
					return
				}
				xidMap[xidStr] = true
				jsonStr, err := json.Marshal(xidMap)
				if err != nil {
					glog.Errorf("Can't encode the value of condition.Reason %s", condition.Reason)
					return
				}
				condition.Reason = string(jsonStr)
			}
		}
		if !conditionFound {
			glog.Infof("XIDCritialError Condition not exists, adding: %v", eventData)
			xidMap := map[string]bool{strconv.FormatUint(eventData, 10): true}
			jsonStr, err := json.Marshal(xidMap)
			if err != nil {
				glog.Errorf("Can't encode the value of xidMap: %v", xidMap)
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
			glog.Errorf("Failed to update node %s status to add XIDCriticalError condition: %v", nodeName, err)
		} else {
			glog.Infof("Successfully add XIDCriticalError condition from node %s.", nodeName)
		}
	}
}

func (hc *GPUHealthChecker) setXIDheartbeat(stopCh <-chan struct{}) {
	hc.updateLastHeartbeatTime()
	interval := hc.heartbeatInterval
	if interval <= 0 {
		interval = defaultHeartbeatInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			hc.updateLastHeartbeatTime()
		}
	}
}

func (hc *GPUHealthChecker) updateLastHeartbeatTime() {
	nodeName := hc.resolveNodeName()
	node, err := hc.kubeClient.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
	if err != nil {
		glog.Errorf("Failed to get node %s for heartbeat update: %v", nodeName, err)
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
		glog.Errorf("Failed to update node %s status to update XIDCondition heartbeat: %v", nodeName, err)
	}
}

func (hc *GPUHealthChecker) recordXIDEvent(e nvmlEvent, cd callDevice) error {
	nodeName := hc.resolveNodeName()
	node, err := hc.kubeClient.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Caught XID error, XID=%d", e.EventData)

	if e.DeviceUUID != "" {
		var affectedGpuIDs []string
		var affectedGpuUUIDs []string

		hc.devMu.Lock()
		deviceIDs := make([]string, 0, len(hc.devices))
		for id := range hc.devices {
			deviceIDs = append(deviceIDs, id)
		}
		sort.Strings(deviceIDs)

		for _, id := range deviceIDs {
			d := hc.devices[id]
			nvmlDev, ok := hc.nvmlDevices[d.ID]
			if !ok || nvmlDev == nil {
				continue
			}
			uuid, ret := nvmlDev.GetUUID()
			if ret != nvml.SUCCESS {
				glog.Errorf("Failed to get UUID for device %s: %v", d.ID, nvmlErrorString(ret))
				continue
			}
			gpu, gi, ci, err := resolveMigDeviceInfo(nvmlDev, uuid, cd)
			if err != nil {
				gpu = uuid
				gi = noInstanceID
				ci = noInstanceID
			}

			if gpu == e.DeviceUUID && gi == e.GpuInstanceID && ci == e.ComputeInstanceID {
				affectedGpuIDs = append(affectedGpuIDs, d.ID)
				affectedGpuUUIDs = append(affectedGpuUUIDs, uuid)
			}
		}
		hc.devMu.Unlock()

		if len(affectedGpuIDs) > 0 {
			fmt.Fprintf(&sb, ", GPU UUID=%s", strings.Join(affectedGpuUUIDs, ", "))
			fmt.Fprintf(&sb, ", Device ID=%s", strings.Join(affectedGpuIDs, ", "))
		} else {
			fmt.Fprintf(&sb, ", GPU UUID=%s", e.DeviceUUID)
		}
	}

	msg := sb.String()
	if hc.recorder != nil {
		hc.recorder.Eventf(node, v1.EventTypeWarning, "XIDError", msg)
	}
	return nil
}

func (hc *GPUHealthChecker) catchError(e nvmlEvent, cd callDevice) {
	// Skip the error if it's not Xid critical
	if e.EventType != nvml.EventTypeXidCriticalError {
		glog.Infof("Skip error Xid=%d as it is not Xid Critical", e.EventData)
		return
	}

	err := hc.recordXIDEvent(e, cd)
	if err != nil {
		glog.Errorf("Failed to record XID=%d for node %s with err %v", e.EventData, hc.resolveNodeName(), err)
	}
	hc.monitorXidevent(e.EventData)

	// Only marking device unhealthy on Double Bit ECC Error or customer-configured codes
	// See https://docs.nvidia.com/deploy/xid-errors/index.html#topic_4
	if _, ok := hc.healthCriticalXid[e.EventData]; !ok {
		glog.Infof("Health checker is skipping Xid %v error", e.EventData)
		return
	}

	if e.DeviceUUID == "" {
		// All devices are unhealthy
		glog.Errorf("XidCriticalError: Xid=%d, All devices will go unhealthy.", e.EventData)
		hc.devMu.Lock()
		toSend := make([]pluginapi.Device, 0, len(hc.devices))
		for id, d := range hc.devices {
			d.Health = pluginapi.Unhealthy
			hc.devices[id] = d
			toSend = append(toSend, d)
		}
		hc.devMu.Unlock()
		for _, d := range toSend {
			select {
			case hc.health <- d:
			case <-hc.stop:
				return
			}
		}
		return
	}

	var toSend []pluginapi.Device
	hc.devMu.Lock()
	for _, d := range hc.devices {
		// Please see https://github.com/NVIDIA/gpu-monitoring-tools/blob/148415f505c96052cb3b7fdf443b34ac853139ec/bindings/go/nvml/nvml.h#L1424
		// for the rationale why gi and ci can be set as such when the UUID is a full GPU UUID and not a MIG device UUID.
		nvmlDev, ok := hc.nvmlDevices[d.ID]
		if !ok || nvmlDev == nil {
			continue
		}
		uuid, ret := nvmlDev.GetUUID()
		if ret != nvml.SUCCESS {
			glog.Errorf("Failed to get UUID for device %s: %v", d.ID, nvmlErrorString(ret))
			continue
		}
		gpu, gi, ci, err := resolveMigDeviceInfo(nvmlDev, uuid, cd)
		if err != nil {
			gpu = uuid
			gi = noInstanceID
			ci = noInstanceID
		}

		if gpu == e.DeviceUUID && gi == e.GpuInstanceID && ci == e.ComputeInstanceID {
			glog.Errorf("XidCriticalError: Xid=%d on Device=%s, uuid=%s, the device will go unhealthy.", e.EventData, d.ID, uuid)
			d.Health = pluginapi.Unhealthy
			hc.devices[d.ID] = d
			toSend = append(toSend, d)
		}
	}
	hc.devMu.Unlock()

	if len(toSend) == 0 {
		glog.Errorf("XidCriticalError: Xid=%d on unknown device.", e.EventData)
		return
	}
	for _, d := range toSend {
		select {
		case hc.health <- d:
		case <-hc.stop:
			return
		}
	}
}

// listenToEvents listens to events from NVML to detect GPU critical errors.
func (hc *GPUHealthChecker) listenToEvents(stopCh <-chan struct{}, eventSet nvmlEventSet) error {
	waitTimeoutMs := hc.eventWaitTimeoutMs
	if waitTimeoutMs == 0 {
		waitTimeoutMs = defaultEventWaitTimeoutMs
	}
	retryDelay := hc.eventRetryDelay
	if retryDelay <= 0 {
		retryDelay = defaultEventRetryDelay
	}
	gd := GPUDevice{nvml: hc.nvmlProvider()}

	for {
		select {
		case <-stopCh:
			return nil
		default:
		}

		e, ret := eventSet.Wait(waitTimeoutMs)
		if ret != nvml.SUCCESS {
			if ret == nvml.ERROR_TIMEOUT {
				continue
			}
			select {
			case <-stopCh:
				return nil
			default:
			}
			glog.Errorf("EventSetWait failed: %v", nvmlErrorString(ret))
			timer := time.NewTimer(retryDelay)
			select {
			case <-stopCh:
				timer.Stop()
				return nil
			case <-timer.C:
			}
			continue
		}

		select {
		case <-stopCh:
			return nil
		default:
		}
		hc.catchError(e, &gd)
	}
}

// Stop stops the listening goroutines and releases the NVML event set.
// Stop is idempotent and does not shut down the long-lived event broadcaster,
// allowing the health checker to be started again after a GPU reset.
func (hc *GPUHealthChecker) Stop() {
	hc.mu.Lock()
	defer hc.mu.Unlock()

	if !hc.running {
		return
	}
	hc.running = false
	close(hc.stop)
	hc.wg.Wait()

	if hc.eventSet != nil {
		if ret := hc.eventSet.Free(); ret != nvml.SUCCESS {
			glog.Errorf("Failed to free NVML event set: %v", nvmlErrorString(ret))
		}
		hc.eventSet = nil
	}
	hc.devMu.Lock()
	hc.nvmlDevices = make(map[string]nvmlDevice)
	hc.devMu.Unlock()
}
