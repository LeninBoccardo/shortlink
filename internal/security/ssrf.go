// Package security hardens the outbound HTTP path. The server fetches
// user-supplied webhook URLs, which is a classic SSRF surface (OWASP A10);
// Validator enforces the scheme allow-list and rejects internal addresses
// (SPEC §9).
package security

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrBlockedURL is returned when a URL resolves to a disallowed address.
var ErrBlockedURL = errors.New("url resolves to a disallowed address")

// dnsLookupTimeout bounds DNS resolution during validation so a hostname whose
// DNS black-holes cannot hang the request path.
const dnsLookupTimeout = 3 * time.Second

// Validator validates webhook URLs and builds SSRF-safe HTTP clients.
type Validator struct {
	// allowEntries holds hostname-or-host:port carve-outs. An entry with an
	// empty port matches any port on that host (backward-compatible with the
	// original "hostname only" semantics); an entry with an explicit port
	// matches only that exact port. The port-aware form is what closes the
	// "loadtest:8091 sink is legitimate, loadtest:8090 control plane is not"
	// pivot: under the legacy hostname-only matcher, allowlisting "loadtest"
	// would let a webhook URL target ANY port on the container, including
	// the unauthenticated /api/keys + /api/attack endpoints.
	allowEntries []allowEntry
	allowNets    []*net.IPNet
}

// allowEntry is a single allowlist carve-out. Empty port means "any port".
type allowEntry struct {
	host string // already lowercased
	port string // "" = wildcard
}

// NewValidator parses SSRF_ALLOWLIST entries — each one of:
//   - a CIDR (e.g. "10.0.0.0/8")
//   - a bare hostname (e.g. "loadtest" — matches any port; the legacy form)
//   - a host:port (e.g. "loadtest:8091" — matches only that exact port)
func NewValidator(allowlist []string) *Validator {
	v := &Validator{}
	for _, entry := range allowlist {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if _, ipnet, err := net.ParseCIDR(entry); err == nil {
			v.allowNets = append(v.allowNets, ipnet)
			continue
		}
		// SplitHostPort succeeds for "host:port"; for a bare hostname it
		// errors out, and we fall through to the "any port" form.
		if host, port, err := net.SplitHostPort(entry); err == nil {
			v.allowEntries = append(v.allowEntries, allowEntry{
				host: strings.ToLower(host),
				port: port,
			})
			continue
		}
		v.allowEntries = append(v.allowEntries, allowEntry{
			host: strings.ToLower(entry),
		})
	}
	return v
}

// matchHostAllow reports whether (host, port) is covered by an allowlist
// entry. An entry with port="" matches any port; otherwise the ports must
// be exactly equal.
func (v *Validator) matchHostAllow(host, port string) bool {
	host = strings.ToLower(host)
	for _, e := range v.allowEntries {
		if e.host != host {
			continue
		}
		if e.port == "" || e.port == port {
			return true
		}
	}
	return false
}

// ValidateURL enforces the http/https scheme allow-list and rejects any host
// that resolves to an internal address, unless the host (or a resolved IP) is
// explicitly allow-listed.
func (v *Validator) ValidateURL(ctx context.Context, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme %q not allowed", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("url has no host")
	}
	// u.Port() is "" for default-scheme ports; normalise to the scheme's
	// well-known port so an allowlist entry like "loadtest:8091" matches a
	// URL written as "http://loadtest:8091/sink".
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}
	if v.matchHostAllow(host, port) {
		return nil
	}
	lookupCtx, cancel := context.WithTimeout(ctx, dnsLookupTimeout)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIP(lookupCtx, "ip", host)
	if err != nil {
		return fmt.Errorf("resolve %q: %w", host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("resolve %q: no addresses", host)
	}
	for _, ip := range ips {
		if err := v.checkIP(ip); err != nil {
			return err
		}
	}
	return nil
}

// checkIP allows public addresses and any internal address covered by an
// allow-listed CIDR; everything else internal is rejected.
func (v *Validator) checkIP(ip net.IP) error {
	if !isInternal(ip) {
		return nil
	}
	for _, n := range v.allowNets {
		if n.Contains(ip) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s", ErrBlockedURL, ip)
}

func isInternal(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsPrivate() || // RFC 1918 (IPv4) + RFC 4193 unique-local (IPv6)
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified()
}

// SafeClient returns an http.Client whose dialer re-resolves and re-validates
// every address at connect time — closing the validate-then-connect TOCTOU
// window against DNS rebinding — and re-validates every redirect hop.
func (v *Validator) SafeClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{
		Proxy: nil, // never route webhook delivery through a proxy
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			if v.matchHostAllow(host, port) {
				return dialer.DialContext(ctx, network, addr)
			}
			lookupCtx, cancel := context.WithTimeout(ctx, dnsLookupTimeout)
			ips, err := net.DefaultResolver.LookupIP(lookupCtx, "ip", host)
			cancel()
			if err != nil {
				return nil, err
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("resolve %q: no addresses", host)
			}
			for _, ip := range ips {
				if err := v.checkIP(ip); err != nil {
					return nil, err
				}
			}
			var lastErr error
			for _, ip := range ips {
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			return nil, lastErr
		},
		TLSHandshakeTimeout: 5 * time.Second,
		// Default per-host cap is 2 — under sustained webhook fan-out to one
		// sink that meant ~98% of attempts opened a fresh TCP+TLS conn.
		// 16 lets keep-alive actually kick in.
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
		// Cap header-arrival at half the overall timeout so body-read has
		// headroom (previously equal to Client.Timeout, leaving 0 for body).
		ResponseHeaderTimeout: timeout / 2,
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return v.ValidateURL(req.Context(), req.URL.String())
		},
	}
}
