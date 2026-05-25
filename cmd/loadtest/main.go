// Command loadtest is the multi-key vegeta attack runner (SPEC §4.4 / §11):
// it loads keys.yaml, spins up one vegeta.Attacker per key against the API
// gateway, hosts a built-in HMAC-verifying webhook sink on :8091 so the
// pipeline closes end-to-end, emits attack_started / attack_complete events
// to the observer hub, and from M6 serves the showcase frontend at :8090
// (embedded into the binary via go:embed). After the attack finishes the
// runner stays up until SIGINT so the user can study the final dashboard.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/leninboccardo/shortlink/internal/events"
	"github.com/leninboccardo/shortlink/internal/keysfile"
)

const shutdownGrace = 5 * time.Second

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
	flag.DurationVar(&cfg.duration, "duration", 60*time.Second, "attack duration")
	flag.StringVar(&cfg.observerURL, "observer", "http://localhost:9090", "observer hub URL")
	flag.StringVar(&cfg.grafanaURL, "grafana", "http://localhost:3000", "Grafana base URL (M6 showcase page)")
	flag.StringVar(&cfg.prometheusURL, "prometheus", "http://localhost:9091", "Prometheus base URL (test console targets-up check)")
	flag.IntVar(&cfg.pagePort, "port", 8090, "showcase page port")
	flag.StringVar(&cfg.sinkURL, "sink-url", "http://localhost:8091/sink", "webhook sink URL advertised to the API")
	flag.IntVar(&cfg.sinkPort, "sink-port", 8091, "webhook sink listen port")
	flag.Parse()
	return cfg
}

func run(cfg runConfig) error {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	keys, err := keysfile.Load(cfg.keysPath)
	if err != nil {
		return err
	}
	if len(keys.Keys) == 0 {
		return fmt.Errorf("%s contains no keys", cfg.keysPath)
	}
	log.Info("loaded keys", "count", len(keys.Keys), "path", cfg.keysPath)

	emitter := events.NewEmitter(events.Config{
		URL:    cfg.observerURL,
		Token:  os.Getenv("OBSERVER_INGEST_TOKEN"),
		Source: events.SourceLoadtest,
		Logger: log,
	})
	defer emitter.Close(2 * time.Second)

	sink := newSink(keys, log)
	sinkSrv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.sinkPort),
		Handler:           sink.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	sinkErr := make(chan error, 1)
	go func() {
		log.Info("webhook sink listening", "addr", sinkSrv.Addr)
		if err := sinkSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			sinkErr <- err
		}
	}()

	page, err := newPageServer(cfg)
	if err != nil {
		return err
	}
	pageBase := fmt.Sprintf("http://localhost:%d", cfg.pagePort)
	tests := newRunner(cfg, keys, cfg.prometheusURL, pageBase)
	// Phase 3: load the scaling catalog from local-limits.yaml. A missing
	// file degrades to "no scaling panel" rather than aborting, so a clone
	// without the limits config (and no `cmd/limits render` run) still
	// boots the rest of the page.
	scaling, err := loadScalingCatalog(cfg.limitsPath, cfg.prometheusURL)
	if err != nil {
		log.Warn("scaling panel disabled", "error", err)
		scaling = nil
	}
	// Bind to loopback only: the page exposes /tests/run/* (which shells
	// `go test` + testcontainers) and /api/scaling-stats (which shells
	// `docker stats`), neither of which is authenticated. Sink stays on all
	// interfaces — container-mode worker reaches it via host.docker.internal.
	pageSrv := &http.Server{
		Addr:              fmt.Sprintf("127.0.0.1:%d", cfg.pagePort),
		Handler:           page.routes(tests, scaling),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	pageErr := make(chan error, 1)
	go func() {
		log.Info("showcase page listening", "addr", pageSrv.Addr,
			"observer", cfg.observerURL, "grafana", cfg.grafanaURL)
		if err := pageSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			pageErr <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	attackCtx, cancelAttack := context.WithTimeout(context.Background(), cfg.duration)
	defer cancelAttack()

	// shutdown closes when the user signals (or a server dies). The watcher
	// goroutine cancels the attack early on signal/error, then signals via
	// close(shutdown). After the attack finishes naturally on duration, the
	// main goroutine waits on shutdown so the page stays up until the user
	// is done browsing the final state.
	shutdown := make(chan struct{})
	go func() {
		defer close(shutdown)
		select {
		case sig := <-stop:
			log.Info("interrupted, cancelling attack", "signal", sig.String())
			cancelAttack()
		case err := <-sinkErr:
			log.Error("sink server failed, cancelling attack", "error", err)
			cancelAttack()
		case err := <-pageErr:
			log.Error("page server failed, cancelling attack", "error", err)
			cancelAttack()
		case <-attackCtx.Done():
			// Attack finished on its own. Wait for the user (or a server
			// failure) before tearing the dashboard down.
			select {
			case sig := <-stop:
				log.Info("signal received post-attack, shutting down", "signal", sig.String())
			case err := <-sinkErr:
				log.Error("sink server failed post-attack", "error", err)
			case err := <-pageErr:
				log.Error("page server failed post-attack", "error", err)
			}
		}
	}()

	emitter.Emit(events.Event{
		Level:   events.LevelInfo,
		Kind:    events.KindAttackStarted,
		Message: fmt.Sprintf("attack started: %d profiles, duration=%s, target=%s", len(keys.Keys), cfg.duration, cfg.target),
		Meta: map[string]any{
			"duration_s": int(cfg.duration.Seconds()),
			"target":     cfg.target,
			"profiles":   len(keys.Keys),
			"sink_url":   cfg.sinkURL,
		},
	})

	results := runAttacks(attackCtx, keys, cfg, log)

	delivered := sink.counts()
	rejected := sink.rejectedCounts()
	printSummary(results, delivered)

	emitter.Emit(events.Event{
		Level:   events.LevelInfo,
		Kind:    events.KindAttackComplete,
		Message: fmt.Sprintf("attack complete: %d profiles", len(results)),
		Meta:    summaryMeta(results, delivered, rejected),
	})

	log.Info("attack done; showcase page still live — Ctrl-C to exit",
		"page", fmt.Sprintf("http://localhost:%d/", cfg.pagePort))

	<-shutdown

	shutCtx, shutCancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer shutCancel()
	if err := sinkSrv.Shutdown(shutCtx); err != nil {
		log.Warn("sink shutdown", "error", err)
	}
	if err := pageSrv.Shutdown(shutCtx); err != nil {
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
