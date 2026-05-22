// Package webhook signs and delivers webhook callbacks to client endpoints.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// Sign produces the X-ShortLink-Signature header value: "sha256=" followed by
// the hex HMAC-SHA256 of body, keyed with the per-key webhook secret (SPEC §8).
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
