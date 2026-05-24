package main

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strings"
)

// webFS holds the showcase page (HTML + CSS + JS) — embedded into the binary
// so `make loadtest` is one command, no `npm install` or external assets.
//
//go:embed web/*
var webFS embed.FS

// pageServer holds the templated index.html bytes and the static-asset FS;
// the index is rendered once at startup from the runner's --observer /
// --grafana flags so the page can connect to the right hosts without any
// hardcoded URL in the embedded file.
type pageServer struct {
	indexHTML []byte
	assets    fs.FS
	csp       string
}

// newPageServer renders index.html with the runtime config baked in. The
// SHORTLINK_CONFIG object is a JSON literal injected into a top-of-page
// <script> — app.js reads window.SHORTLINK_CONFIG synchronously on load.
func newPageServer(cfg runConfig) (*pageServer, error) {
	// Refuse to start with a malformed --grafana flag. The page injects the
	// value as an iframe src; a javascript: URL would execute, and even
	// well-formed http(s) URLs are sandbox-restricted by the embedded HTML.
	// Empty is allowed -- the JS shows a fallback hint.
	if cfg.grafanaURL != "" &&
		!strings.HasPrefix(cfg.grafanaURL, "http://") &&
		!strings.HasPrefix(cfg.grafanaURL, "https://") {
		return nil, fmt.Errorf("--grafana must be http:// or https:// (got %q)", cfg.grafanaURL)
	}
	rawHTML, err := fs.ReadFile(webFS, "web/index.html")
	if err != nil {
		return nil, fmt.Errorf("read embedded index.html: %w", err)
	}
	tmpl, err := template.New("index").Parse(string(rawHTML))
	if err != nil {
		return nil, fmt.Errorf("parse index.html: %w", err)
	}
	pageCfg := map[string]string{
		"observer_url": cfg.observerURL,
		"observer_ws":  observerWSURL(cfg.observerURL),
		"grafana_url":  cfg.grafanaURL,
		"sink_url":     cfg.sinkURL,
	}
	cfgJSON, err := json.Marshal(pageCfg)
	if err != nil {
		return nil, fmt.Errorf("marshal page config: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]template.JS{
		"Config": template.JS(cfgJSON),
	}); err != nil {
		return nil, fmt.Errorf("render index.html: %w", err)
	}
	assets, err := fs.Sub(webFS, "web")
	if err != nil {
		return nil, fmt.Errorf("scope to web/: %w", err)
	}
	csp, err := buildCSP(buf.Bytes(), cfg.observerURL, cfg.grafanaURL)
	if err != nil {
		return nil, err
	}
	return &pageServer{indexHTML: buf.Bytes(), assets: assets, csp: csp}, nil
}

// buildCSP returns a Content-Security-Policy string tailored to the rendered
// index. The inline <script> that injects window.SHORTLINK_CONFIG is allowed
// via its sha256 hash (CSP3) so we don't need 'unsafe-inline'; connect-src
// includes the observer WebSocket URL; frame-src includes the optional
// Grafana base. Strict everywhere else.
func buildCSP(html []byte, observerURL, grafanaURL string) (string, error) {
	const openTag, closeTag = "<script>", "</script>"
	start := bytes.Index(html, []byte(openTag))
	end := bytes.Index(html, []byte(closeTag))
	if start < 0 || end < 0 || start >= end {
		return "", fmt.Errorf("inline <script> block not found in rendered index.html")
	}
	inline := html[start+len(openTag) : end]
	sum := sha256.Sum256(inline)
	scriptHash := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"

	connect := []string{"'self'", observerWSURL(observerURL)}
	if observerURL != "" {
		connect = append(connect, observerURL)
	}
	frame := []string{"'self'"}
	if grafanaURL != "" {
		frame = append(frame, grafanaURL)
	}
	return strings.Join([]string{
		"default-src 'none'",
		"script-src 'self' " + scriptHash,
		"style-src 'self'",
		"img-src 'self' data:",
		"connect-src " + strings.Join(connect, " "),
		"frame-src " + strings.Join(frame, " "),
		"form-action 'none'",
		"base-uri 'none'",
	}, "; "), nil
}

// routes returns an http.Handler that serves the rendered index at /, the
// embedded web/ assets, and -- when a runner is passed -- the test-console
// /tests/* endpoints. The /tests/* routes must be registered BEFORE the "/"
// catch-all or http.ServeMux would shadow them with the asset handler.
func (p *pageServer) routes(testRunner *runner) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	if testRunner != nil {
		testRunner.attachRoutes(mux)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Headers shared by both the templated index and the static-asset
		// fallback: stop MIME sniffing and refuse to be framed.
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if r.URL.Path == "/" {
			w.Header().Set("Content-Security-Policy", p.csp)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(p.indexHTML)
			return
		}
		http.FileServer(http.FS(p.assets)).ServeHTTP(w, r)
	})
	return mux
}

// observerWSURL turns http://host:port into ws://host:port/stream (https → wss).
func observerWSURL(observerURL string) string {
	switch {
	case strings.HasPrefix(observerURL, "https://"):
		return "wss://" + strings.TrimPrefix(observerURL, "https://") + "/stream"
	case strings.HasPrefix(observerURL, "http://"):
		return "ws://" + strings.TrimPrefix(observerURL, "http://") + "/stream"
	default:
		return observerURL + "/stream"
	}
}
