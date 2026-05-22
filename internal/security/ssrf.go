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

// Validator validates webhook URLs and builds SSRF-safe HTTP clients.
type Validator struct {
	allowHosts map[string]struct{}
	allowNets  []*net.IPNet
}

// NewValidator parses SSRF_ALLOWLIST entries — each either a hostname or a
// CIDR — that are exempt from the internal-IP rejection.
func NewValidator(allowlist []string) *Validator {
	v := &Validator{allowHosts: make(map[string]struct{})}
	for _, entry := range allowlist {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if _, ipnet, err := net.ParseCIDR(entry); err == nil {
			v.allowNets = append(v.allowNets, ipnet)
			continue
		}
		v.allowHosts[strings.ToLower(entry)] = struct{}{}
	}
	return v
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
	if _, ok := v.allowHosts[strings.ToLower(host)]; ok {
		return nil
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
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
			if _, ok := v.allowHosts[strings.ToLower(host)]; ok {
				return dialer.DialContext(ctx, network, addr)
			}
			ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
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
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: timeout,
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
