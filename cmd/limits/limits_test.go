package main

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestIntOrAuto_UnmarshalYAML(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantAuto bool
		wantVal int
		wantErr bool
	}{
		{"auto literal", `auto`, true, 0, false},
		{"auto uppercase", `AUTO`, true, 0, false},
		{"integer", `4`, false, 4, false},
		{"zero rejected", `0`, false, 0, true},
		{"negative rejected", `-1`, false, 0, true},
		{"bogus string", `hello`, false, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var v IntOrAuto
			err := yaml.Unmarshal([]byte(tc.yaml), &v)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got value %+v", v)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if v.Auto != tc.wantAuto || v.Value != tc.wantVal {
				t.Errorf("got {Auto:%v Value:%d}, want {Auto:%v Value:%d}", v.Auto, v.Value, tc.wantAuto, tc.wantVal)
			}
		})
	}
}

func TestValidateBudget_FitsAuto(t *testing.T) {
	cfg := &Config{
		Host: Host{
			MaxTotalCPUs:     IntOrAuto{Auto: true},
			MaxTotalMemoryMB: IntOrAuto{Auto: true},
		},
		Services: map[string]Service{
			"api":    {CPU: 0.5, MemoryMB: 256, MaxReplicas: 1},
			"worker": {CPU: 1.0, MemoryMB: 512, MaxReplicas: 2},
		},
	}
	// Host: 8 CPUs, 8192 MB. Headroom 80% = 6.4 CPU / 6553 MB. Demand = 2.5 CPU
	// (0.5*1 + 1.0*2) / 1280 MB. Fits.
	if err := ValidateBudget(cfg, HostCapacity{LogicalCPUs: 8, MemoryMB: 8192}); err != nil {
		t.Fatalf("expected fit, got: %v", err)
	}
}

func TestValidateBudget_OverByMemory(t *testing.T) {
	cfg := &Config{
		Host: Host{
			MaxTotalCPUs:     IntOrAuto{Auto: true},
			MaxTotalMemoryMB: IntOrAuto{Value: 1024},
		},
		Services: map[string]Service{
			"worker": {CPU: 0.1, MemoryMB: 600, MaxReplicas: 4}, // 2400 MB > 1024
		},
	}
	err := ValidateBudget(cfg, HostCapacity{LogicalCPUs: 8, MemoryMB: 8192})
	if err == nil {
		t.Fatal("expected over-budget error, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"total memory", "worker", "max_total_"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error missing %q in:\n%s", want, msg)
		}
	}
}

func TestValidateBudget_MaxReplicasDefaultsToOne(t *testing.T) {
	cfg := &Config{
		Host: Host{
			MaxTotalCPUs:     IntOrAuto{Value: 2},
			MaxTotalMemoryMB: IntOrAuto{Value: 2048},
		},
		Services: map[string]Service{
			"postgres": {CPU: 1.0, MemoryMB: 512}, // no MaxReplicas -> treat as 1
		},
	}
	if err := ValidateBudget(cfg, HostCapacity{LogicalCPUs: 4, MemoryMB: 4096}); err != nil {
		t.Fatalf("expected fit (singletons should count as replicas=1), got: %v", err)
	}
}

func TestTrimZero(t *testing.T) {
	cases := map[float64]string{
		0.5:   "0.5",
		1.0:   "1",
		0.25:  "0.25",
		0.125: "0.125",
		2:     "2",
	}
	for in, want := range cases {
		if got := trimZero(in); got != want {
			t.Errorf("trimZero(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateServices(t *testing.T) {
	cases := []struct {
		name    string
		svcs    map[string]Service
		wantErr string // substring; empty => no error expected
	}{
		{"ok", map[string]Service{"api": {CPU: 0.5, MemoryMB: 256, MaxReplicas: 2}}, ""},
		{"zero cpu", map[string]Service{"api": {CPU: 0, MemoryMB: 256}}, "cpu must be > 0"},
		{"negative cpu", map[string]Service{"api": {CPU: -1, MemoryMB: 256}}, "cpu must be > 0"},
		{"zero mem", map[string]Service{"api": {CPU: 0.5, MemoryMB: 0}}, "memory_mb must be > 0"},
		{"negative mem", map[string]Service{"api": {CPU: 0.5, MemoryMB: -10}}, "memory_mb must be > 0"},
		{"negative replicas", map[string]Service{"api": {CPU: 0.5, MemoryMB: 256, MaxReplicas: -1}}, "max_replicas must be >= 0"},
		{"huge replicas", map[string]Service{"api": {CPU: 0.5, MemoryMB: 256, MaxReplicas: 9999}}, "max_replicas must be <= 100"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateServices(tc.svcs)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q missing %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestMilliCPUAndMebibytes(t *testing.T) {
	if got := milliCPU(0.5); got != "500m" {
		t.Errorf("milliCPU(0.5)=%q want 500m", got)
	}
	if got := milliCPU(1.0); got != "1000m" {
		t.Errorf("milliCPU(1.0)=%q want 1000m", got)
	}
	if got := mebibytes(256); got != "256Mi" {
		t.Errorf("mebibytes(256)=%q want 256Mi", got)
	}
}
