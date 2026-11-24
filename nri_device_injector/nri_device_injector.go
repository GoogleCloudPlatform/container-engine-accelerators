// Copyright 2023 Google Inc. All Rights Reserved.
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
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/sys/unix"
	"sigs.k8s.io/yaml"

	"github.com/containerd/nri/pkg/api"
	"github.com/containerd/nri/pkg/stub"
	log "github.com/sirupsen/logrus"
)

const (
	deviceKeyPrefix    = "devices.gke.io"
	ctrDeviceKeyPrefix = deviceKeyPrefix + "/container."
	pluginName         = "device_injector_nri"
	pluginIdx          = "10"
	charDevice         = "c"
)

var (
	allowedNamespacesFlag = flag.String(
		"allowed-namespaces",
		"*",
		`Comma-separated namespaces whose pods may use devices.gke.io/container.* annotations. Empty ("") rejects every annotation, "*" allows all namespaces.`,
	)
	allowMIGCapabilitiesFlag = flag.Bool(
		"allow-mig-capabilities",
		false,
		"Allow injecting /dev/nvidia-caps/nvidia-cap[0-9]+ devices from pod annotations.",
	)

	baseDevicePathPattern = regexp.MustCompile(`^/dev/(nvidia[0-9]+|nvidiactl|nvidia-uvm|dmabuf_import_helper)$`)
	migCapDevicePattern   = regexp.MustCompile(`^/dev/nvidia-caps/nvidia-cap[0-9]+$`)
)

type device struct {
	Path string `json:"path"`
}

type lstatFunc func(string, *unix.Stat_t) error

type plugin struct {
	stub              stub.Stub
	lstat             lstatFunc
	allowedNamespaces map[string]bool
	allowMIGCaps      bool
}

func main() {
	flag.Parse()

	opts := []stub.Option{
		stub.WithPluginName(pluginName),
		stub.WithPluginIdx(pluginIdx),
	}

	p := &plugin{
		lstat:             unix.Lstat,
		allowedNamespaces: parseAllowedNamespaces(*allowedNamespacesFlag),
		allowMIGCaps:      *allowMIGCapabilitiesFlag,
	}
	var err error
	if p.stub, err = stub.New(p, append(opts, stub.WithOnClose(p.onClose))...); err != nil {
		log.Errorf("Failed to create plugin stub: %v", err)
		os.Exit(1)
	}

	if err := p.stub.Run(context.Background()); err != nil {
		log.Errorf("plugin exited with error %v", err)
		os.Exit(1)
	}
}

func (p *plugin) onClose() {
	log.Info("NRI connection closed")
}

func parseAllowedNamespaces(raw string) map[string]bool {
	allowed := make(map[string]bool)
	for _, part := range strings.Split(raw, ",") {
		if ns := strings.TrimSpace(part); ns != "" {
			allowed[ns] = true
		}
	}
	return allowed
}

func (p *plugin) namespaceAllowed(ns string) bool {
	return ns != "" && (p.allowedNamespaces["*"] || p.allowedNamespaces[ns])
}

// CreateContainer handles CreateContainer requests from containerd NRI.
func (p *plugin) CreateContainer(_ context.Context, pod *api.PodSandbox, container *api.Container) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	if pod == nil || container == nil {
		return nil, nil, nil
	}

	ctrName := container.Name
	l := log.WithFields(log.Fields{"container": ctrName, "pod": pod.Name, "namespace": pod.Namespace})
	defer l.Info("Finished CreateContainer")
	l.Info("Started CreateContainer")

	rawAnnotation, hasAnnotation := pod.Annotations[ctrDeviceKeyPrefix+ctrName]
	if !hasAnnotation || strings.TrimSpace(rawAnnotation) == "" {
		l.Debug("No devices annotated")
		return &api.ContainerAdjustment{}, nil, nil
	}

	if !p.namespaceAllowed(pod.Namespace) {
		err := fmt.Errorf("namespace %q is not allowed to use %s annotations", pod.Namespace, ctrDeviceKeyPrefix)
		l.WithError(err).Warn("Rejected device injection")
		return nil, nil, err
	}

	devices, err := parseDevices(ctrName, pod.Annotations, p.allowMIGCaps)
	if err != nil {
		l.WithError(err).Warn("Failed to parse devices from pod annotation")
		return nil, nil, err
	}
	adjust := &api.ContainerAdjustment{}
	if len(devices) == 0 {
		l.Debug("No devices annotated")
		return adjust, nil, nil
	}

	for _, d := range devices {
		l.WithField("device", d.Path).Info("Annotated device")
		deviceNRI, err := toNRIDevice(d.Path, p.allowMIGCaps, p.lstat)
		if err != nil {
			l.WithField("device", d.Path).WithError(err).Warn("Failed to resolve host device")
			return nil, nil, err
		}
		adjust.AddDevice(deviceNRI)
		l.WithField("device", d.Path).Info("Injected device")
	}
	return adjust, nil, nil
}

func parseDevices(ctrName string, podAnnotations map[string]string, allowMIGCaps bool) ([]device, error) {
	deviceKey := ctrDeviceKeyPrefix + ctrName
	value, ok := podAnnotations[deviceKey]
	if !ok {
		return nil, nil
	}

	var parsedDevices []device
	if err := yaml.UnmarshalStrict([]byte(value), &parsedDevices); err != nil {
		return nil, fmt.Errorf("invalid device annotation %q: %w", deviceKey, err)
	}

	seen := make(map[string]bool, len(parsedDevices))
	var devices []device
	for _, d := range parsedDevices {
		if err := validateDevicePath(d.Path, allowMIGCaps); err != nil {
			return nil, fmt.Errorf("invalid device annotation %q: %v", deviceKey, err)
		}
		if seen[d.Path] {
			continue
		}
		seen[d.Path] = true
		devices = append(devices, d)
	}
	return devices, nil
}

func validateDevicePath(path string, allowMIGCaps bool) error {
	if path == "" {
		return fmt.Errorf("device path cannot be empty")
	}
	if filepath.Clean(path) != path {
		return fmt.Errorf("device path %q must be canonical", path)
	}
	if baseDevicePathPattern.MatchString(path) {
		return nil
	}
	if allowMIGCaps && migCapDevicePattern.MatchString(path) {
		return nil
	}
	return fmt.Errorf("device path %q is not in the allowlist", path)
}

func toNRIDevice(path string, allowMIGCaps bool, lstat lstatFunc) (*api.LinuxDevice, error) {
	if err := validateDevicePath(path, allowMIGCaps); err != nil {
		return nil, err
	}
	if lstat == nil {
		lstat = unix.Lstat
	}

	// Reject symlinked parent directories under /dev.
	if dir := filepath.Dir(path); dir != "/dev" {
		var dirStat unix.Stat_t
		if err := lstat(dir, &dirStat); err != nil {
			return nil, fmt.Errorf("lstat parent directory %q: %v", dir, err)
		}
		if (dirStat.Mode & unix.S_IFMT) != unix.S_IFDIR {
			return nil, fmt.Errorf("parent path %q is not a directory (mode %#o)", dir, dirStat.Mode)
		}
	}

	var stat unix.Stat_t
	if err := lstat(path, &stat); err != nil {
		return nil, fmt.Errorf("lstat device path %q: %v", path, err)
	}
	if (stat.Mode & unix.S_IFMT) != unix.S_IFCHR {
		return nil, fmt.Errorf("device path %q is not a character device (mode %#o)", path, stat.Mode)
	}

	devNumber := uint64(stat.Rdev)
	fileMode := os.FileMode(stat.Mode) & os.ModePerm
	return &api.LinuxDevice{
		Path:     path,
		Type:     charDevice,
		Major:    int64(unix.Major(devNumber)),
		Minor:    int64(unix.Minor(devNumber)),
		FileMode: api.FileMode(fileMode),
		Uid:      api.UInt32(stat.Uid),
		Gid:      api.UInt32(stat.Gid),
	}, nil
}
