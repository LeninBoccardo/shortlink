// Package auth generates and validates API keys and webhook signing secrets.
package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/leninboccardo/shortlink/internal/shortener"
)

const (
	keyPrefix    = "sl_live_"
	secretPrefix = "whsec_"
	randomLen    = 32
	hintLen      = 6
)

// NewAPIKey returns a fresh raw API key: sl_live_<32 base62 chars> (SPEC §9).
// The raw key is shown once and never stored — only its hash is persisted.
func NewAPIKey() (string, error) {
	r, err := shortener.Generate(randomLen)
	if err != nil {
		return "", fmt.Errorf("generate api key: %w", err)
	}
	return keyPrefix + r, nil
}

// NewWebhookSecret returns a fresh webhook signing secret:
// whsec_<32 base62 chars> (SPEC §8/§9).
func NewWebhookSecret() (string, error) {
	r, err := shortener.Generate(randomLen)
	if err != nil {
		return "", fmt.Errorf("generate webhook secret: %w", err)
	}
	return secretPrefix + r, nil
}

// HashKey returns the hex-encoded SHA-256 of a raw API key — the form stored
// in Postgres.
func HashKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// Hint returns the last 6 characters of a raw key, for non-sensitive display.
func Hint(raw string) string {
	if len(raw) <= hintLen {
		return raw
	}
	return raw[len(raw)-hintLen:]
}

// ValidKeyFormat reports whether raw matches the sl_live_<32 base62> shape. It
// is a cheap pre-check the validator runs before hitting Postgres so a flood
// of obviously-malformed keys (random bytes, header noise, attacker probes)
// never reaches the api_keys table.
//
// Why: failed lookups are deliberately not cached (cache-pollution risk), so
// every garbage key would otherwise cost one PG round-trip. Rejecting on shape
// before the cache/DB collapses that cost to a couple of bounds checks.
func ValidKeyFormat(raw string) bool {
	if len(raw) != len(keyPrefix)+randomLen {
		return false
	}
	if raw[:len(keyPrefix)] != keyPrefix {
		return false
	}
	for i := len(keyPrefix); i < len(raw); i++ {
		c := raw[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		default:
			return false
		}
	}
	return true
}
