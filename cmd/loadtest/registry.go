package main

import (
	"fmt"
	"strings"
	"sync"

	"github.com/leninboccardo/shortlink/internal/auth"
	"github.com/leninboccardo/shortlink/internal/keysfile"
)

// keyRegistry is the loadtest binary's in-memory mirror of config/keys.yaml,
// guarded by a single RWMutex. The previous design passed *keysfile.File
// around directly, which was fine when keys were boot-only; the operator UI
// can now add/remove keys at runtime, so every reader (test runner, attack
// loop) and writer (control-plane handlers) needs to coordinate.
//
// Two invariants the registry preserves:
//   - The on-disk keys.yaml is rewritten on every mutation under the same
//     lock, so a crash mid-mutation leaves a consistent file (keysfile.Write
//     is itself atomic via tempfile + rename).
//   - Snapshots returned by readers are independent copies — callers can
//     iterate without holding the lock, and a concurrent revoke can't
//     yank an entry out from under them mid-iteration.
type keyRegistry struct {
	mu   sync.RWMutex
	path string
	file keysfile.File
}

// newKeyRegistry seeds the registry from path. A missing file is treated as
// an empty registry (keysfile.Load already encodes that policy) so a fresh
// dev environment without a generated keys.yaml still boots.
func newKeyRegistry(path string) (*keyRegistry, error) {
	loaded, err := keysfile.Load(path)
	if err != nil {
		return nil, err
	}
	return &keyRegistry{path: path, file: *loaded}, nil
}

// Snapshot returns an independent copy of the current entries. Callers
// must not assume the returned slice tracks future mutations.
func (r *keyRegistry) Snapshot() []keysfile.Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]keysfile.Entry, len(r.file.Keys))
	copy(out, r.file.Keys)
	return out
}

// FindByTier returns the first entry matching tier (case-insensitive). The
// "first" choice is deliberate: the operator UI surfaces tier requirements
// per test card, and "first key of this tier" is a stable, predictable
// answer when multiple keys share a tier.
func (r *keyRegistry) FindByTier(tier string) (keysfile.Entry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, k := range r.file.Keys {
		if strings.EqualFold(k.Tier, tier) {
			return k, true
		}
	}
	return keysfile.Entry{}, false
}

// SecretByHint returns the webhook signing secret for the key matching hint.
// Used by the webhook sink to verify the HMAC on incoming deliveries — and
// the registry-aware lookup means UI-generated keys are verifiable too
// (was a Phase B TODO; folded back into A so the sink doesn't lie about
// the keys it knows).
func (r *keyRegistry) SecretByHint(hint string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, k := range r.file.Keys {
		if auth.Hint(k.Key) == hint {
			return k.WebhookSecret, true
		}
	}
	return "", false
}

// Append adds entry to the registry and atomically rewrites keys.yaml. A
// duplicate-by-hint check guards against the operator double-clicking
// Generate mid-IO; the DB has a UNIQUE constraint on key_hash that catches
// truly concurrent CreateAPIKey calls upstream, this is just the in-process
// mirror.
func (r *keyRegistry) Append(entry keysfile.Entry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	hint := auth.Hint(entry.Key)
	for _, k := range r.file.Keys {
		if auth.Hint(k.Key) == hint {
			return fmt.Errorf("key with hint %s already registered", hint)
		}
	}
	r.file.Keys = append(r.file.Keys, entry)
	if err := keysfile.Write(r.path, &r.file); err != nil {
		// Roll the in-memory change back so the registry stays consistent
		// with disk; the caller will surface the disk error to the user.
		r.file.Keys = r.file.Keys[:len(r.file.Keys)-1]
		return err
	}
	return nil
}

// RemoveByHint drops the entry whose key matches hint and atomically
// rewrites keys.yaml. Returns false if no entry matched (caller decides
// whether that's a 404 or a silent success).
func (r *keyRegistry) RemoveByHint(hint string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	idx := -1
	for i, k := range r.file.Keys {
		if auth.Hint(k.Key) == hint {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false, nil
	}
	removed := r.file.Keys[idx]
	r.file.Keys = append(r.file.Keys[:idx], r.file.Keys[idx+1:]...)
	if err := keysfile.Write(r.path, &r.file); err != nil {
		// Restore the entry to keep memory/disk consistent.
		r.file.Keys = append(r.file.Keys[:idx], append([]keysfile.Entry{removed}, r.file.Keys[idx:]...)...)
		return false, err
	}
	return true, nil
}
