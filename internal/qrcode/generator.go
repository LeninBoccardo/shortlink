// Package qrcode renders QR code PNGs for short URLs and derives their
// object-storage keys.
package qrcode

import (
	"fmt"
	"time"

	qr "github.com/skip2/go-qrcode"
)

// Generate renders content as a PNG QR code, size×size px, with medium error
// correction (SPEC §4.2).
func Generate(content string, size int) ([]byte, error) {
	png, err := qr.Encode(content, qr.Medium, size)
	if err != nil {
		return nil, fmt.Errorf("encode qr: %w", err)
	}
	return png, nil
}

// ObjectKey builds the object-storage key for a job's QR PNG, partitioned by
// date: {year}/{month}/{day}/{job_id}.png (SPEC §6).
func ObjectKey(jobID string, at time.Time) string {
	at = at.UTC()
	return fmt.Sprintf("%04d/%02d/%02d/%s.png", at.Year(), int(at.Month()), at.Day(), jobID)
}
