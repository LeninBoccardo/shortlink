package main

import (
	"context"
	"errors"
	"net"
	"strings"

	"github.com/leninboccardo/shortlink/internal/security"
)

// errClass reduces an arbitrary worker error to one of a small fixed set of
// labels safe to ship to the observer (and from there to any WebSocket
// client). The full error keeps going to the local slog for debugging —
// only the broadcast surface is sanitized (SPEC §10 / audit S5).
//
// Adding a new class? Keep them coarse (no hostnames, no IPs, no URLs).
func errClass(err error) string {
	if err == nil {
		return "none"
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, security.ErrBlockedURL):
		// Sentinel match: stable across error-message rewording.
		return "ssrf_blocked"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	// Fall back to substring matches on the rendered error — covers wrapped
	// dial errors, body-read errors, and the dispatcher's HTTP-status errors
	// without leaking the URL/host that produced them.
	s := err.Error()
	switch {
	case strings.Contains(s, "connection refused"):
		return "connection_refused"
	case strings.Contains(s, "no such host"):
		return "dns"
	case strings.Contains(s, "unexpected status"), strings.Contains(s, "status code"):
		return "http_status"
	case strings.Contains(s, "EOF"):
		return "eof"
	}
	return "internal"
}
