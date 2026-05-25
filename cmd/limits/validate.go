package main

import (
	"fmt"
	"sort"
	"strings"
)

// hostHeadroomPct is what fraction of detected capacity "auto" allows the
// stack to claim. Leaves room for the OS, browsers, IDE, and other dev tools.
const hostHeadroomPct = 0.80

// ValidateBudget sums per-service resource demand and compares it against the
// host budget (or detected capacity if budget is "auto"). Returns an
// actionable error when over capacity, listing the largest contributors.
func ValidateBudget(cfg *Config, host HostCapacity) error {
	capCPU := float64(host.LogicalCPUs) * hostHeadroomPct
	if !cfg.Host.MaxTotalCPUs.Auto {
		capCPU = float64(cfg.Host.MaxTotalCPUs.Value)
	}
	capMem := int(float64(host.MemoryMB) * hostHeadroomPct)
	if !cfg.Host.MaxTotalMemoryMB.Auto {
		capMem = cfg.Host.MaxTotalMemoryMB.Value
	}

	type row struct {
		name string
		cpu  float64
		mem  int
		reps int
	}
	var rows []row
	var sumCPU float64
	var sumMem int
	for name, s := range cfg.Services {
		reps := s.MaxReplicas
		if reps == 0 {
			reps = 1
		}
		rows = append(rows, row{name, s.CPU * float64(reps), s.MemoryMB * reps, reps})
		sumCPU += s.CPU * float64(reps)
		sumMem += s.MemoryMB * reps
	}

	if sumCPU <= capCPU && sumMem <= capMem {
		return nil
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].mem > rows[j].mem })
	var b strings.Builder
	fmt.Fprintf(&b, "config exceeds host budget\n")
	fmt.Fprintf(&b, "  total CPU:    %.2f vs cap %.2f (host has %d logical CPUs)\n", sumCPU, capCPU, host.LogicalCPUs)
	fmt.Fprintf(&b, "  total memory: %d MB vs cap %d MB (host has %d MB)\n", sumMem, capMem, host.MemoryMB)
	b.WriteString("  largest contributors (cpu*replicas, memory*replicas):\n")
	for i, r := range rows {
		if i >= 3 {
			break
		}
		fmt.Fprintf(&b, "    %-12s cpu=%.2f mem=%d MB replicas=%d\n", r.name, r.cpu, r.mem, r.reps)
	}
	b.WriteString("  reduce services.<name>.{cpu,memory_mb,max_replicas} or raise host.max_total_*")
	return fmt.Errorf("%s", b.String())
}
