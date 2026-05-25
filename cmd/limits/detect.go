package main

import (
	"fmt"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/mem"
)

// HostCapacity is what we read off the local machine.
type HostCapacity struct {
	LogicalCPUs int `json:"logical_cpus"`
	MemoryMB    int `json:"memory_mb"`
}

// DetectHost reads CPU count and physical memory cross-platform via gopsutil.
// gopsutil's cpu and mem packages are already pulled transitively by
// testcontainers-go, so this adds no new dependencies.
func DetectHost() (HostCapacity, error) {
	n, err := cpu.Counts(true)
	if err != nil {
		return HostCapacity{}, fmt.Errorf("cpu.Counts: %w", err)
	}
	vm, err := mem.VirtualMemory()
	if err != nil {
		return HostCapacity{}, fmt.Errorf("mem.VirtualMemory: %w", err)
	}
	return HostCapacity{
		LogicalCPUs: n,
		MemoryMB:    int(vm.Total / (1024 * 1024)),
	}, nil
}
