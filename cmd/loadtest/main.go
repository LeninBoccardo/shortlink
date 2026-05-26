// Command loadtest hosts the showcase frontend at :8090 and the multi-key
// vegeta attack engine (SPEC §4.4 / §11). Originally a one-shot CLI that
// fired an attack at boot and stayed up for inspection; from the v1 operator-
// panel work it is now driven by the embedded UI — POST /api/keys/generate
// to provision a key, POST /api/attack/start to begin, POST /api/attack/stop
// to halt. The boot-time auto-attack is gone: the UI is the canonical entry
// point and a CLI flag default is too easy to start by accident.
//
// The binary still ships its built-in HMAC-verifying webhook sink on :8091
// so the pipeline closes end-to-end during an attack, and still emits
// attack_started / attack_complete events to the observer.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/leninboccardo/shortlink/internal/db"
	"github.com/leninboccardo/shortlink/internal/events"
	"github.com/leninboccardo/shortlink/internal/storage"
)

const (
	shutdownGrace     = 5 * time.Second
	loadtestPoolSize  = 2 // small — only keygen + revoke touch the DB
	startupCtxTimeout = 10 * time.Second
)

type runConfig struct {
	keysPath      string
	limitsPath    string
	target        string
	duration      time.Duration
	observerURL   string
	grafanaURL    string
	prometheusURL string
	pagePort      int
	sinkURL       string
	sinkPort      int
	databaseURL   string
	containerMode bool
}

func main() {
	cfg := parseFlags()
	if err := run(cfg); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func parseFlags() runConfig {
	var cfg runConfig
	flag.StringVar(&cfg.keysPath, "keys", "config/keys.yaml", "path to keys.yaml")
	flag.StringVar(&cfg.limitsPath, "limits", "config/local-limits.yaml", "path to local-limits.yaml (scaling panel source)")
	flag.StringVar(&cfg.target, "target", "http://localhost:8080", "API gateway base URL")
	flag.DurationVar(&cfg.duration, "duration", 60*time.Second, "default attack duration (POST /api/attack/start can override)")
	flag.StringVar(&cfg.observerURL, "observer", "http://localhost:9090", "observer hub URL")
	flag.StringVar(&cfg.grafanaURL, "grafana", "http://localhost:3000", "Grafana base URL (M6 showcase page)")
	flag.StringVar(&cfg.prometheusURL, "prometheus", "http://localhost:9091", "Prometheus base URL (test console targets-up check)")
	flag.IntVar(&cfg.pagePort, "port", 8090, "showcase page port")
	flag.StringVar(&cfg.sinkURL, "sink-url", "http://localhost:8091/sink", "webhook sink URL advertised to the API")
	flag.IntVar(&cfg.sinkPort, "sink-port", 8091, "webhook sink listen port")
	// DATABASE_URL falls back to the env default the api/worker use so a
	// host-mode loadtest doesn't need its own flag plumbing in the setup
	// scripts. In compose.full mode, deploy/docker-compose.full.yml sets
	// DATABASE_URL on the loadtest service to the in-network pgbouncer.
	flag.StringVar(&cfg.databaseURL, "database-url", envOr("DATABASE_URL", "postgres://shortlink:shortlink@localhost:16432/shortlink?sslmode=disable"), "Postgres DSN for keygen + revoke")
	flag.Parse()
	cfg.containerMode = envBool("CONTAINER_MODE")
	return cfg
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envBool(key string) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return false
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false
	}
	return b
}

func run(cfg runConfig) error {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	// Boot the key registry from keys.yaml. A missing file is OK -- it just
	// means the operator panel starts empty and the user clicks Generate
	// before running any /shorten test.
	registry, err := newKeyRegistry(cfg.keysPath)
	if err != nil {
		return err
	}
	log.Info("loaded keys", "count", len(registry.Snapshot()), "path", cfg.keysPath)

	// DB pool: tiny, only used by the operator panel's keygen + revoke
	// handlers. Failing to connect here aborts startup — without DB access
	// the UI's headline feature is broken; better to surface that loudly
	// than to boot a half-functional showcase page.
	startupCtx, cancelStartup := context.WithTimeout(context.Background(), startupCtxTimeout)
	defer cancelStartup()
	pool, err := storage.NewPool(startupCtx, cfg.databaseURL, loadtestPoolSize)
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	defer pool.Close()
	queries := db.New(pool)

	emitter := events.NewEmitter(events.Config{
		URL:    cfg.observerURL,
		Token:  os.Getenv("OBSERVER_INGEST_TOKEN"),
		Source: events.SourceLoadtest,
		Logger: log,
	})
	defer emitter.Close(2 * time.Second)

	// The sink resolves HMAC secrets through the registry on every delivery
	// (registry.SecretByHint), so UI-generated keys whose first webhook
	// arrives mid-session are still verified — no rebuild-on-mutate dance.
	sinkSrv := newSink(registry, log)
	sinkHTTP := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.sinkPort),
		Handler:           sinkSrv.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	sinkErr := make(chan error, 1)
	go func() {
		log.Info("webhook sink listening", "addr", sinkHTTP.Addr)
		if err := sinkHTTP.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			sinkErr <- err
		}
	}()

	page, err := newPageServer(cfg)
	if err != nil {
		return err
	}
	pageBase := fmt.Sprintf("http://localhost:%d", cfg.pagePort)
	tests := newRunner(cfg, registry, cfg.prometheusURL, pageBase)
	scaling, err := loadScalingCatalog(cfg.limitsPath, cfg.prometheusURL)
	if err != nil {
		log.Warn("scaling panel disabled", "error", err)
		scaling = nil
	}
	control := newControlServer(registry, queries, cfg, log, emitter, sinkSrv)

	// Bind to loopback only: /api/keys/generate is an unauthenticated
	// keygen oracle, /api/attack/start triggers a vegeta storm, and
	// /tests/run/* shells `go test`. Same threat model as before -- adding
	// the new endpoints didn't change it.
	pageHTTP := &http.Server{
		Addr:              fmt.Sprintf("127.0.0.1:%d", cfg.pagePort),
		Handler:           page.routes(tests, scaling, control),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	pageErr := make(chan error, 1)
	go func() {
		log.Info("showcase page listening", "addr", pageHTTP.Addr,
			"observer", cfg.observerURL, "grafana", cfg.grafanaURL,
			"container_mode", cfg.containerMode)
		if err := pageHTTP.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			pageErr <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	select {
	case sig := <-stop:
		log.Info("interrupted, shutting down", "signal", sig.String())
	case err := <-sinkErr:
		log.Error("sink server failed", "error", err)
	case err := <-pageErr:
		log.Error("page server failed", "error", err)
	}

	shutCtx, shutCancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer shutCancel()
	if err := sinkHTTP.Shutdown(shutCtx); err != nil {
		log.Warn("sink shutdown", "error", err)
	}
	if err := pageHTTP.Shutdown(shutCtx); err != nil {
		log.Warn("page shutdown", "error", err)
	}
	return nil
}

// summaryMeta packs the per-key vegeta totals into a single map for the
// attack_complete meta — the observer logs the message and the showcase page
// can drill into the per-profile breakdown.
func summaryMeta(results []attackResult, delivered, rejected map[string]int) map[string]any {
	profiles := make([]map[string]any, 0, len(results))
	for _, r := range results {
		hint := hintOf(r.Profile.Key)
		profiles = append(profiles, map[string]any{
			"name":      r.Profile.Name,
			"tier":      r.Profile.Tier,
			"requests":  r.Metrics.Requests,
			"success":   r.Metrics.Success,
			"p99_ms":    r.Metrics.Latencies.P99.Milliseconds(),
			"delivered": delivered[hint],
			"rejected":  rejected[hint],
		})
	}
	return map[string]any{
		"profiles": profiles,
	}
}
