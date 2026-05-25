package main

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the top-level config/local-limits.yaml document.
type Config struct {
	Host     Host               `yaml:"host"`
	Services map[string]Service `yaml:"services"`
}

// Host caps the total resources the local stack may consume.
type Host struct {
	MaxTotalCPUs     IntOrAuto `yaml:"max_total_cpus"`
	MaxTotalMemoryMB IntOrAuto `yaml:"max_total_memory_mb"`
}

// Service is one row under services:. max_replicas is ignored for infra
// services (postgres/redis/etc.) -- they're singletons in compose.
type Service struct {
	CPU         float64 `yaml:"cpu"`
	MemoryMB    int     `yaml:"memory_mb"`
	MaxReplicas int     `yaml:"max_replicas,omitempty"`
}

// IntOrAuto parses either an integer or the literal string "auto". When Auto
// is true, validation substitutes the detected host capacity (clamped to
// hostHeadroomPct so the OS and other dev tools still have room).
type IntOrAuto struct {
	Auto  bool
	Value int
}

func (i *IntOrAuto) UnmarshalYAML(node *yaml.Node) error {
	if node.Value == "auto" {
		i.Auto = true
		return nil
	}
	var n int
	if err := node.Decode(&n); err != nil {
		return fmt.Errorf("expected integer or \"auto\", got %q", node.Value)
	}
	i.Value = n
	return nil
}

// LoadConfig reads and parses config/local-limits.yaml.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(c.Services) == 0 {
		return nil, fmt.Errorf("%s: services block is empty or missing", path)
	}
	return &c, nil
}
