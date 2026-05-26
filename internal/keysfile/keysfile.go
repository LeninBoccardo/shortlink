// Package keysfile reads and writes the load-test runner's keys.yaml (SPEC §4.4).
//
// The file holds raw API key material — it's gitignored and must not leave the
// dev machine. Both cmd/keygen (writer) and cmd/loadtest (reader + UI-driven
// writer) use these types so the schema lives in one place.
package keysfile

import (
	"fmt"
	"os"
	"path/filepath"

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

// Load reads and parses keys.yaml from path. A non-existent file is NOT an
// error — it returns an empty File so callers (the loadtest binary at boot,
// the operator UI on first run) can treat "no file yet" the same as "file
// with zero keys" without sprinkling errors.Is(os.ErrNotExist) checks.
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &File{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &f, nil
}

// Write marshals f to YAML and writes it to path with mode 0o600 (it contains
// raw secrets). Writes are atomic: marshal into a sibling temp file then
// os.Rename, so a mid-write crash never leaves a truncated keys.yaml that
// would 401 every request next boot.
func Write(path string, f *File) error {
	data, err := yaml.Marshal(f)
	if err != nil {
		return fmt.Errorf("marshal keys: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	// Temp file in the same directory so os.Rename is a same-filesystem move
	// (atomic on POSIX, and on NTFS for files that aren't held open).
	tmp, err := os.CreateTemp(dir, ".keys.yaml.tmp.*")
	if err != nil {
		return fmt.Errorf("create temp keys file: %w", err)
	}
	tmpPath := tmp.Name()
	// Make sure we clean up the temp file on any error path. os.Remove on a
	// path that was already renamed away is a benign no-op.
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp keys file: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp keys file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp keys file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename temp keys file to %s: %w", path, err)
	}
	return nil
}
