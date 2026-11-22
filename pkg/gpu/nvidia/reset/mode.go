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

package reset

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/GoogleCloudPlatform/container-engine-accelerators/pkg/gpu/nvidia/mig"
)

type (
	Mode interface {
		DeviceIDs(gpu PhysicalGPU) []string
		PreReset(ctx context.Context, gpus []PhysicalGPU) error
		Converge(ctx context.Context, gpus []PhysicalGPU) error
	}
	MIGDevices interface {
		PartitionIDs(gpu string) []string
		DiscoverGPU(gpu, partitionSize string) error
	}
	ModeOption func(*modeImpl)
	modeImpl   struct {
		cfg  Config
		mig  MIGDevices
		nvml NVML
		run  executor
	}
)

func WithMIGDevices(m MIGDevices) ModeOption { return func(o *modeImpl) { o.mig = m } }
func WithNVML(nvml NVML) ModeOption          { return func(o *modeImpl) { o.nvml = nvml } }
func NewMode(cfg Config, opts ...ModeOption) Mode {
	m := &modeImpl{cfg: cfg, run: execExecutor{}}
	for _, opt := range opts {
		if opt != nil {
			opt(m)
		}
	}
	m.nvml = cmp.Or[NVML](m.nvml, NewNVML())
	return m
}
func resolvePCIBusID(nvml NVML, gpu PhysicalGPU) (string, error) {
	if _, err := parseMinor(gpu); err != nil || nvml == nil {
		return "", cmp.Or(err, fmt.Errorf("resolve PCI bus ID for %s: nil NVML", gpu))
	}
	busID, err := nvml.PCIBusID(gpu)
	if busID = strings.TrimSpace(busID); err != nil || busID == "" {
		return "", fmt.Errorf("resolve PCI bus ID for %s: %w", gpu, cmp.Or(err, errors.New("empty PCI bus ID")))
	}
	return busID, nil
}
func (m *modeImpl) smi(ctx context.Context, args ...string) ([]byte, error) {
	return m.run.Run(ctx, nvidiaSMI, args...)
}
func (m *modeImpl) DeviceIDs(gpu PhysicalGPU) []string {
	base := []string{string(gpu)}
	if m.cfg.PartitionSize != "" {
		if m.mig == nil {
			return nil
		}
		base = m.mig.PartitionIDs(string(gpu))
	}
	if m.cfg.MaxSharedClientsPerGPU <= 0 {
		return base
	}
	var ids []string
	for _, b := range base {
		for i := 0; i < m.cfg.MaxSharedClientsPerGPU; i++ {
			ids = append(ids, fmt.Sprintf("%s/vgpu%d", b, i))
		}
	}
	return ids
}
func (m *modeImpl) PreReset(ctx context.Context, gpus []PhysicalGPU) error {
	for _, gpu := range gpus {
		if m.cfg.PartitionSize == "" {
			break
		}
		pciBusID, err := resolvePCIBusID(m.nvml, gpu)
		if err != nil {
			return err
		}
		if err := mig.DestroyPartitions(ctx, m.smi, pciBusID); err != nil {
			var inUse *mig.InstanceInUseError
			if errors.As(err, &inUse) {
				cp := *inUse
				cp.GPU = string(gpu)
				if _, after, ok := strings.Cut(cp.Instance, "/gi"); ok {
					cp.Instance = string(gpu) + "/gi" + after
				}
				return &Abort{Code: CodeGPUBusy, Message: cp.Error()}
			}
			return fmt.Errorf("mig pre-reset %s: %w", gpu, err)
		}
	}
	return nil
}
func (m *modeImpl) Converge(ctx context.Context, gpus []PhysicalGPU) error {
	if m.cfg.PartitionSize != "" && len(gpus) > 0 {
		if m.mig == nil {
			return errors.New("mig mode: device manager is not configured")
		}
		if _, err := mig.BuildPartitionStr(m.cfg.PartitionSize); err != nil {
			return fmt.Errorf("mig converge: %w", err)
		}
		for _, gpu := range gpus {
			pciBusID, err := resolvePCIBusID(m.nvml, gpu)
			if err != nil {
				return err
			}
			if mig.IsGPUPartitioned(ctx, m.smi, pciBusID, m.cfg.PartitionSize) && m.mig.DiscoverGPU(string(gpu), m.cfg.PartitionSize) == nil {
				continue
			}
			if err := mig.DestroyPartitions(ctx, m.smi, pciBusID); err != nil {
				return fmt.Errorf("mig converge destroy %s: %w", gpu, err)
			}
			if err := mig.CreatePartitions(ctx, m.smi, pciBusID, m.cfg.PartitionSize); err != nil {
				return fmt.Errorf("mig converge create %s: %w", gpu, err)
			}
			if err := m.mig.DiscoverGPU(string(gpu), m.cfg.PartitionSize); err != nil {
				return fmt.Errorf("mig converge discover %s: %w", gpu, err)
			}
		}
	}
	return nil
}
