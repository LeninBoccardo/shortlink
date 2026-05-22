// Package shortener generates short URL slugs and validates client-supplied
// custom slugs.
package shortener

import (
	"crypto/rand"
	"errors"
	"fmt"
)

const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

const (
	customSlugMinLen = 1
	customSlugMaxLen = 64
)

// Generate returns a cryptographically random base62 string of length n.
// Bytes are rejection-sampled so every character is uniformly distributed
// (no modulo bias).
func Generate(n int) (string, error) {
	if n <= 0 {
		return "", errors.New("slug length must be positive")
	}
	const limit = 256 - (256 % len(alphabet)) // reject bytes >= limit
	out := make([]byte, n)
	buf := make([]byte, 1)
	for i := 0; i < n; {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("read random: %w", err)
		}
		if int(buf[0]) >= limit {
			continue
		}
		out[i] = alphabet[int(buf[0])%len(alphabet)]
		i++
	}
	return string(out), nil
}

// ValidateCustomSlug checks a client-supplied slug: 1–64 characters drawn
// from [A-Za-z0-9_-].
func ValidateCustomSlug(s string) error {
	if len(s) < customSlugMinLen || len(s) > customSlugMaxLen {
		return fmt.Errorf("custom slug must be %d–%d characters", customSlugMinLen, customSlugMaxLen)
	}
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '-' || r == '_'
		if !ok {
			return errors.New("custom slug may contain only letters, digits, '-' and '_'")
		}
	}
	return nil
}
