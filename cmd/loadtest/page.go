package main

import (
	"bytes"
	"embed"
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
	return &pageServer{indexHTML: buf.Bytes(), assets: assets}, nil
}

// routes returns an http.Handler that serves the rendered index at / and the
// rest of the embedded web/ tree (app.css, app.js, future assets) as static
// files.
func (p *pageServer) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
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
