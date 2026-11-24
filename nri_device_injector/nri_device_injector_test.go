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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/containerd/nri/pkg/api"
	"github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
)

func TestParseDevices(t *testing.T) {
	tests := []struct {
		name         string
		container    string
		annotations  map[string]string
		allowMIGCaps bool
		want         []device
		wantErr      bool
	}{
		{
			name:      "nil_annotations",
			container: "test",
		},
		{
			name:        "empty_annotations_map",
			container:   "test",
			annotations: map[string]string{},
		},
		{
			name:        "empty_annotation_value",
			container:   "test",
			annotations: map[string]string{"devices.gke.io/container.test": ""},
		},
		{
			name:        "unrelated_annotation",
			container:   "test",
			annotations: map[string]string{"other.io/key": "val"},
		},
		{
			name:      "valid_yaml_default_allowlisted_paths",
			container: "test",
			annotations: map[string]string{
				"devices.gke.io/container.test": `
- path: /dev/nvidia0
- path: /dev/nvidia15
- path: /dev/nvidiactl
- path: /dev/nvidia-uvm
- path: /dev/dmabuf_import_helper
`,
			},
			want: []device{
				{Path: "/dev/nvidia0"},
				{Path: "/dev/nvidia15"},
				{Path: "/dev/nvidiactl"},
				{Path: "/dev/nvidia-uvm"},
				{Path: "/dev/dmabuf_import_helper"},
			},
		},
		{
			name:      "rejects_mig_caps_by_default",
			container: "test",
			annotations: map[string]string{
				"devices.gke.io/container.test": "- path: /dev/nvidia-caps/nvidia-cap1\n",
			},
			wantErr: true,
		},
		{
			name:         "allows_mig_caps_when_opted_in",
			container:    "test",
			allowMIGCaps: true,
			annotations: map[string]string{
				"devices.gke.io/container.test": "- path: /dev/nvidia-caps/nvidia-cap1\n",
			},
			want: []device{
				{Path: "/dev/nvidia-caps/nvidia-cap1"},
			},
		},
		{
			name:      "valid_json_array_format",
			container: "tcpxo-daemon",
			annotations: map[string]string{
				"devices.gke.io/container.tcpxo-daemon": `[{"path":"/dev/nvidia0"},{"path":"/dev/dmabuf_import_helper"}]`,
			},
			want: []device{
				{Path: "/dev/nvidia0"},
				{Path: "/dev/dmabuf_import_helper"},
			},
		},
		{
			name:      "deduplicates_repeated_paths",
			container: "test",
			annotations: map[string]string{
				"devices.gke.io/container.test": `
- path: /dev/nvidia0
- path: /dev/nvidiactl
- path: /dev/nvidia0
`,
			},
			want: []device{
				{Path: "/dev/nvidia0"},
				{Path: "/dev/nvidiactl"},
			},
		},
		{
			name:      "rejects_empty_path",
			container: "test",
			annotations: map[string]string{
				"devices.gke.io/container.test": "- path: \"\"\n",
			},
			wantErr: true,
		},
		{
			name:      "rejects_arbitrary_block_device",
			container: "test",
			annotations: map[string]string{
				"devices.gke.io/container.test": "- path: /dev/sda\n",
			},
			wantErr: true,
		},
		{
			name:      "rejects_dev_mem",
			container: "test",
			annotations: map[string]string{
				"devices.gke.io/container.test": "- path: /dev/mem\n",
			},
			wantErr: true,
		},
		{
			name:      "rejects_dev_fuse",
			container: "test",
			annotations: map[string]string{
				"devices.gke.io/container.test": "- path: /dev/fuse\n",
			},
			wantErr: true,
		},
		{
			name:      "rejects_dev_infiniband_dir_and_subpath",
			container: "test",
			annotations: map[string]string{
				"devices.gke.io/container.test": "- path: /dev/infiniband/uverbs0\n",
			},
			wantErr: true,
		},
		{
			name:         "rejects_nvidia_caps_dir_itself",
			container:    "test",
			allowMIGCaps: true,
			annotations: map[string]string{
				"devices.gke.io/container.test": "- path: /dev/nvidia-caps\n",
			},
			wantErr: true,
		},
		{
			name:      "rejects_path_traversal",
			container: "test",
			annotations: map[string]string{
				"devices.gke.io/container.test": "- path: /dev/nvidia0/../sda\n",
			},
			wantErr: true,
		},
		{
			name:      "rejects_non_canonical_dot_segment",
			container: "test",
			annotations: map[string]string{
				"devices.gke.io/container.test": "- path: /dev/./nvidia0\n",
			},
			wantErr: true,
		},
		{
			name:      "rejects_file_mode_override",
			container: "test",
			annotations: map[string]string{
				"devices.gke.io/container.test": `
- path: /dev/nvidia0
  file_mode: 438
`,
			},
			wantErr: true,
		},
		{
			name:      "rejects_uid_override",
			container: "test",
			annotations: map[string]string{
				"devices.gke.io/container.test": `
- path: /dev/nvidia0
  uid: 1000
`,
			},
			wantErr: true,
		},
		{
			name:      "rejects_gid_override",
			container: "test",
			annotations: map[string]string{
				"devices.gke.io/container.test": `
- path: /dev/nvidia0
  gid: 1000
`,
			},
			wantErr: true,
		},
		{
			name:      "rejects_major_minor_type_overrides",
			container: "test",
			annotations: map[string]string{
				"devices.gke.io/container.test": `
- path: /dev/nvidia0
  type: b
  major: 8
  minor: 0
`,
			},
			wantErr: true,
		},
		{
			name:      "rejects_invalid_yaml",
			container: "test",
			annotations: map[string]string{
				"devices.gke.io/container.test": `
- path: /dev/nvidia0
path: /dev/nvidiactl
`,
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDevices(tc.container, tc.annotations, tc.allowMIGCaps)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseDevices(%q, %v, %v) error = %v, wantErr %v", tc.container, tc.annotations, tc.allowMIGCaps, err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("parseDevices(%q, %v, %v) diff (-want +got):\n%s", tc.container, tc.annotations, tc.allowMIGCaps, diff)
			}
		})
	}
}

func TestToNRIDevice(t *testing.T) {
	fakeStat := func(entries map[string]unix.Stat_t) lstatFunc {
		return func(p string, st *unix.Stat_t) error {
			entry, ok := entries[p]
			if !ok {
				return unix.ENOENT
			}
			*st = entry
			return nil
		}
	}

	tests := []struct {
		name         string
		path         string
		allowMIGCaps bool
		stats        map[string]unix.Stat_t
		want         *api.LinuxDevice
		wantErr      bool
	}{
		{
			name: "nvidia0_char_device_root_0666",
			path: "/dev/nvidia0",
			stats: map[string]unix.Stat_t{
				"/dev/nvidia0": {
					Mode: unix.S_IFCHR | 0o666,
					Rdev: unix.Mkdev(195, 0),
					Uid:  0,
					Gid:  0,
				},
			},
			want: &api.LinuxDevice{
				Path:     "/dev/nvidia0",
				Type:     "c",
				Major:    195,
				Minor:    0,
				FileMode: api.FileMode(os.FileMode(0o666)),
				Uid:      api.UInt32(uint32(0)),
				Gid:      api.UInt32(uint32(0)),
			},
		},
		{
			name:         "nvidia_cap_preserves_restricted_mode_uid_gid_when_opted_in",
			path:         "/dev/nvidia-caps/nvidia-cap1",
			allowMIGCaps: true,
			stats: map[string]unix.Stat_t{
				"/dev/nvidia-caps": {
					Mode: unix.S_IFDIR | 0o755,
				},
				"/dev/nvidia-caps/nvidia-cap1": {
					Mode: unix.S_IFCHR | 0o440,
					Rdev: unix.Mkdev(235, 1),
					Uid:  0,
					Gid:  44,
				},
			},
			want: &api.LinuxDevice{
				Path:     "/dev/nvidia-caps/nvidia-cap1",
				Type:     "c",
				Major:    235,
				Minor:    1,
				FileMode: api.FileMode(os.FileMode(0o440)),
				Uid:      api.UInt32(uint32(0)),
				Gid:      api.UInt32(uint32(44)),
			},
		},
		{
			name: "rejects_nvidia_cap_when_mig_flag_disabled",
			path: "/dev/nvidia-caps/nvidia-cap1",
			stats: map[string]unix.Stat_t{
				"/dev/nvidia-caps": {
					Mode: unix.S_IFDIR | 0o755,
				},
				"/dev/nvidia-caps/nvidia-cap1": {
					Mode: unix.S_IFCHR | 0o440,
					Rdev: unix.Mkdev(235, 1),
				},
			},
			wantErr: true,
		},
		{
			name: "rejects_symlink_device",
			path: "/dev/nvidia0",
			stats: map[string]unix.Stat_t{
				"/dev/nvidia0": {
					Mode: unix.S_IFLNK | 0o777,
				},
			},
			wantErr: true,
		},
		{
			name:         "rejects_symlink_parent_dir",
			path:         "/dev/nvidia-caps/nvidia-cap1",
			allowMIGCaps: true,
			stats: map[string]unix.Stat_t{
				"/dev/nvidia-caps": {
					Mode: unix.S_IFLNK | 0o777,
				},
				"/dev/nvidia-caps/nvidia-cap1": {
					Mode: unix.S_IFCHR | 0o666,
					Rdev: unix.Mkdev(235, 1),
				},
			},
			wantErr: true,
		},
		{
			name: "rejects_block_device",
			path: "/dev/nvidia0",
			stats: map[string]unix.Stat_t{
				"/dev/nvidia0": {
					Mode: unix.S_IFBLK | 0o660,
					Rdev: unix.Mkdev(8, 0),
				},
			},
			wantErr: true,
		},
		{
			name: "rejects_directory",
			path: "/dev/nvidia0",
			stats: map[string]unix.Stat_t{
				"/dev/nvidia0": {
					Mode: unix.S_IFDIR | 0o755,
				},
			},
			wantErr: true,
		},
		{
			name:    "rejects_disallowed_path_even_when_called_directly",
			path:    "/dev/sda",
			stats:   map[string]unix.Stat_t{},
			wantErr: true,
		},
		{
			name:    "rejects_missing_device",
			path:    "/dev/nvidiactl",
			stats:   map[string]unix.Stat_t{},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := toNRIDevice(tc.path, tc.allowMIGCaps, fakeStat(tc.stats))
			if (err != nil) != tc.wantErr {
				t.Fatalf("toNRIDevice(%q, %v) error = %v, wantErr %v", tc.path, tc.allowMIGCaps, err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if diff := cmp.Diff(tc.want, got, cmp.Comparer(proto.Equal)); diff != "" {
				t.Errorf("toNRIDevice(%q, %v) diff (-want +got):\n%s", tc.path, tc.allowMIGCaps, diff)
			}
		})
	}
}

func TestToNRIDeviceRealFilesystemRejections(t *testing.T) {
	devRoot := t.TempDir()
	lstatInTempDir := func(p string, st *unix.Stat_t) error {
		rel := strings.TrimPrefix(p, "/dev")
		return unix.Lstat(filepath.Join(devRoot, rel), st)
	}

	regFile := filepath.Join(devRoot, "nvidiactl")
	if err := os.WriteFile(regFile, []byte("not-a-device"), 0o644); err != nil {
		t.Fatalf("os.WriteFile(%q) failed: %v", regFile, err)
	}
	if _, err := toNRIDevice("/dev/nvidiactl", false, lstatInTempDir); err == nil {
		t.Errorf("toNRIDevice(%q) on regular file = nil, want error", "/dev/nvidiactl")
	}

	symlinkPath := filepath.Join(devRoot, "nvidia0")
	if err := os.Symlink("/dev/null", symlinkPath); err != nil {
		t.Fatalf("os.Symlink(%q) failed: %v", symlinkPath, err)
	}
	if _, err := toNRIDevice("/dev/nvidia0", false, lstatInTempDir); err == nil {
		t.Errorf("toNRIDevice(%q) on symlink = nil, want error", "/dev/nvidia0")
	}

	targetDir := t.TempDir()
	parentSymlink := filepath.Join(devRoot, "nvidia-caps")
	if err := os.Symlink(targetDir, parentSymlink); err != nil {
		t.Fatalf("os.Symlink(%q) failed: %v", parentSymlink, err)
	}
	if _, err := toNRIDevice("/dev/nvidia-caps/nvidia-cap1", true, lstatInTempDir); err == nil {
		t.Errorf("toNRIDevice(%q) with symlinked parent = nil, want error", "/dev/nvidia-caps/nvidia-cap1")
	}
}

func TestToNRIDeviceRootMknod(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Skipping TestToNRIDeviceRootMknod as mknod requires root privileges.")
	}
	devRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(devRoot, "nvidia-caps"), 0o755); err != nil {
		t.Fatalf("os.MkdirAll failed: %v", err)
	}
	lstatInTempDir := func(p string, st *unix.Stat_t) error {
		rel := strings.TrimPrefix(p, "/dev")
		return unix.Lstat(filepath.Join(devRoot, rel), st)
	}

	charPath := filepath.Join(devRoot, "nvidia0")
	if err := unix.Mknod(charPath, unix.S_IFCHR|0o660, int(unix.Mkdev(195, 0))); err != nil {
		t.Fatalf("unix.Mknod(%q) failed: %v", charPath, err)
	}
	if err := os.Chmod(charPath, 0o660); err != nil {
		t.Fatalf("os.Chmod(%q) failed: %v", charPath, err)
	}
	got, err := toNRIDevice("/dev/nvidia0", false, lstatInTempDir)
	if err != nil {
		t.Fatalf("toNRIDevice(%q) failed: %v", "/dev/nvidia0", err)
	}
	want := &api.LinuxDevice{
		Path:     "/dev/nvidia0",
		Type:     "c",
		Major:    195,
		Minor:    0,
		FileMode: api.FileMode(os.FileMode(0o660)),
		Uid:      api.UInt32(uint32(0)),
		Gid:      api.UInt32(uint32(0)),
	}
	if diff := cmp.Diff(want, got, cmp.Comparer(proto.Equal)); diff != "" {
		t.Errorf("toNRIDevice(%q) diff (-want +got):\n%s", "/dev/nvidia0", diff)
	}

	blockPath := filepath.Join(devRoot, "nvidiactl")
	if err := unix.Mknod(blockPath, unix.S_IFBLK|0o660, int(unix.Mkdev(8, 0))); err != nil {
		t.Fatalf("unix.Mknod(%q) failed: %v", blockPath, err)
	}
	if _, err := toNRIDevice("/dev/nvidiactl", false, lstatInTempDir); err == nil {
		t.Errorf("toNRIDevice(%q) on block device = nil, want error", "/dev/nvidiactl")
	}
}

func TestCreateContainer(t *testing.T) {
	fakeLstat := func(path string, st *unix.Stat_t) error {
		switch path {
		case "/dev/nvidia0":
			*st = unix.Stat_t{Mode: unix.S_IFCHR | 0o666, Rdev: unix.Mkdev(195, 0), Uid: 0, Gid: 0}
			return nil
		case "/dev/dmabuf_import_helper":
			*st = unix.Stat_t{Mode: unix.S_IFCHR | 0o660, Rdev: unix.Mkdev(10, 123), Uid: 0, Gid: 44}
			return nil
		case "/dev/nvidia-caps":
			*st = unix.Stat_t{Mode: unix.S_IFDIR | 0o755}
			return nil
		case "/dev/nvidia-caps/nvidia-cap1":
			*st = unix.Stat_t{Mode: unix.S_IFCHR | 0o440, Rdev: unix.Mkdev(235, 1), Uid: 0, Gid: 44}
			return nil
		default:
			return fmt.Errorf("unexpected path %q", path)
		}
	}

	nvidia0Dev := &api.LinuxDevice{
		Path:     "/dev/nvidia0",
		Type:     "c",
		Major:    195,
		Minor:    0,
		FileMode: api.FileMode(os.FileMode(0o666)),
		Uid:      api.UInt32(uint32(0)),
		Gid:      api.UInt32(uint32(0)),
	}
	dmabufDev := &api.LinuxDevice{
		Path:     "/dev/dmabuf_import_helper",
		Type:     "c",
		Major:    10,
		Minor:    123,
		FileMode: api.FileMode(os.FileMode(0o660)),
		Uid:      api.UInt32(uint32(0)),
		Gid:      api.UInt32(uint32(44)),
	}
	migCap1Dev := &api.LinuxDevice{
		Path:     "/dev/nvidia-caps/nvidia-cap1",
		Type:     "c",
		Major:    235,
		Minor:    1,
		FileMode: api.FileMode(os.FileMode(0o440)),
		Uid:      api.UInt32(uint32(0)),
		Gid:      api.UInt32(uint32(44)),
	}

	tests := []struct {
		name              string
		allowedNamespaces map[string]bool
		allowMIGCaps      bool
		pod               *api.PodSandbox
		container         *api.Container
		wantAdjust        *api.ContainerAdjustment
		wantErr           bool
	}{
		{
			name:              "disallowed_namespace_requesting_nvidia0_rejected",
			allowedNamespaces: parseAllowedNamespaces("gpudirect-system,workload-ns"),
			pod: &api.PodSandbox{
				Name:      "tenant-pod",
				Namespace: "untrusted-ns",
				Annotations: map[string]string{
					"devices.gke.io/container.app": "- path: /dev/nvidia0\n",
				},
			},
			container: &api.Container{Name: "app"},
			wantErr:   true,
		},
		{
			name:              "allowed_namespace_gets_requested_devices",
			allowedNamespaces: parseAllowedNamespaces("gpudirect-system,workload-ns"),
			pod: &api.PodSandbox{
				Name:      "gpudirect-pod",
				Namespace: "workload-ns",
				Annotations: map[string]string{
					"devices.gke.io/container.tcpxo-daemon": "- path: /dev/nvidia0\n- path: /dev/dmabuf_import_helper\n",
				},
			},
			container: &api.Container{Name: "tcpxo-daemon"},
			wantAdjust: &api.ContainerAdjustment{
				Linux: &api.LinuxContainerAdjustment{
					Devices: []*api.LinuxDevice{nvidia0Dev, dmabufDev},
				},
			},
		},
		{
			name:              "unset_namespace_allowlist_rejects_annotated_pod",
			allowedNamespaces: parseAllowedNamespaces(""),
			pod: &api.PodSandbox{
				Name:      "pod-a",
				Namespace: "default",
				Annotations: map[string]string{
					"devices.gke.io/container.app": "- path: /dev/nvidia0\n",
				},
			},
			container: &api.Container{Name: "app"},
			wantErr:   true,
		},
		{
			name:              "unannotated_pod_succeeds_even_with_unset_allowlist",
			allowedNamespaces: nil,
			pod: &api.PodSandbox{
				Name:      "kube-dns",
				Namespace: "kube-system",
			},
			container:  &api.Container{Name: "kubedns"},
			wantAdjust: &api.ContainerAdjustment{},
		},
		{
			name:              "wildcard_namespace_allows_non_empty_namespace",
			allowedNamespaces: parseAllowedNamespaces("*"),
			pod: &api.PodSandbox{
				Name:      "vertex-pod",
				Namespace: "project-1234",
				Annotations: map[string]string{
					"devices.gke.io/container.tcpx-daemon": "- path: /dev/nvidia0\n",
				},
			},
			container: &api.Container{Name: "tcpx-daemon"},
			wantAdjust: &api.ContainerAdjustment{
				Linux: &api.LinuxContainerAdjustment{
					Devices: []*api.LinuxDevice{nvidia0Dev},
				},
			},
		},
		{
			name:              "wildcard_namespace_still_rejects_empty_pod_namespace",
			allowedNamespaces: parseAllowedNamespaces("*"),
			pod: &api.PodSandbox{
				Name: "nameless-ns-pod",
				Annotations: map[string]string{
					"devices.gke.io/container.app": "- path: /dev/nvidia0\n",
				},
			},
			container: &api.Container{Name: "app"},
			wantErr:   true,
		},
		{
			name:              "mig_capabilities_rejected_when_flag_off",
			allowedNamespaces: parseAllowedNamespaces("default"),
			allowMIGCaps:      false,
			pod: &api.PodSandbox{
				Name:      "mig-pod",
				Namespace: "default",
				Annotations: map[string]string{
					"devices.gke.io/container.app": "- path: /dev/nvidia-caps/nvidia-cap1\n",
				},
			},
			container: &api.Container{Name: "app"},
			wantErr:   true,
		},
		{
			name:              "mig_capabilities_injected_when_flag_on",
			allowedNamespaces: parseAllowedNamespaces("default"),
			allowMIGCaps:      true,
			pod: &api.PodSandbox{
				Name:      "mig-pod",
				Namespace: "default",
				Annotations: map[string]string{
					"devices.gke.io/container.app": "- path: /dev/nvidia-caps/nvidia-cap1\n",
				},
			},
			container: &api.Container{Name: "app"},
			wantAdjust: &api.ContainerAdjustment{
				Linux: &api.LinuxContainerAdjustment{
					Devices: []*api.LinuxDevice{migCap1Dev},
				},
			},
		},
		{
			name:              "disallowed_device_path_rejected_in_allowed_namespace",
			allowedNamespaces: parseAllowedNamespaces("default"),
			pod: &api.PodSandbox{
				Name:      "attacker-pod",
				Namespace: "default",
				Annotations: map[string]string{
					"devices.gke.io/container.app": "- path: /dev/sda\n",
				},
			},
			container: &api.Container{Name: "app"},
			wantErr:   true,
		},
		{
			name:              "missing_host_device_rejected",
			allowedNamespaces: parseAllowedNamespaces("default"),
			pod: &api.PodSandbox{
				Name:      "missing-dev-pod",
				Namespace: "default",
				Annotations: map[string]string{
					"devices.gke.io/container.app": "- path: /dev/nvidiactl\n",
				},
			},
			container: &api.Container{Name: "app"},
			wantErr:   true,
		},
		{
			name:              "nil_pod_returns_nil_without_error",
			allowedNamespaces: parseAllowedNamespaces("default"),
			pod:               nil,
			container:         &api.Container{Name: "app"},
			wantAdjust:        nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &plugin{
				lstat:             fakeLstat,
				allowedNamespaces: tc.allowedNamespaces,
				allowMIGCaps:      tc.allowMIGCaps,
			}
			gotAdjust, gotUpdates, err := p.CreateContainer(context.Background(), tc.pod, tc.container)
			if (err != nil) != tc.wantErr {
				t.Errorf("CreateContainer() error = %v, wantErr %v", err, tc.wantErr)
				return
			}
			if tc.wantErr {
				if gotAdjust != nil {
					t.Errorf("CreateContainer() adjustment on error = %v, want nil", gotAdjust)
				}
				return
			}
			if len(gotUpdates) != 0 {
				t.Errorf("CreateContainer() updates = %v, want empty", gotUpdates)
			}
			if diff := cmp.Diff(tc.wantAdjust, gotAdjust, cmp.Comparer(proto.Equal)); diff != "" {
				t.Errorf("CreateContainer() adjustment diff (-want +got):\n%s", diff)
			}
		})
	}
}
