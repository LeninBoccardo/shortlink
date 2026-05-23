// Package keysfile reads and writes the load-test runner's keys.yaml (SPEC §4.4).
//
// The file holds raw API key material — it's gitignored and must not leave the
// dev machine. Both cmd/keygen (writer) and cmd/loadtest (reader) use these
// types so the schema lives in one place.
package keysfile

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Entry is one row of keys.yaml.
type Entry struct {
	Name             string `yaml:"name"`
	Key              string `yaml:"key"`
	WebhookSecret    string `yaml:"webhook_secret"`
	AttackRatePerMin int    `yaml:"attack_rate_per_min"`
	Tier             string `yaml:"tier"`
}

// File is the top-level keys.yaml document.
type File struct {
	Keys []Entry `yaml:"keys"`
}

// Load reads and parses keys.yaml from path.
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &f, nil
}

// Write marshals f to YAML and writes it to path with mode 0o600 (it contains
// raw secrets).
func Write(path string, f *File) error {
	data, err := yaml.Marshal(f)
	if err != nil {
		return fmt.Errorf("marshal keys: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
