// Command loadtest is the multi-key vegeta attack runner (SPEC §4.4): it
// loads keys.yaml, spins up one vegeta.Attacker per key against the API
// gateway, hosts a built-in HMAC-verifying webhook sink on :8091 so the
// pipeline closes end-to-end, and emits attack_started / attack_complete
// events to the observer hub.
//
// M5 is a one-shot CLI: it runs for --duration, prints per-key metrics, and
// exits. The :8090 showcase page lands in M6.
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
	keysPath    string
	target      string
	duration    time.Duration
	observerURL string
	grafanaURL  string
	pagePort    int
	sinkURL     string
	sinkPort    int
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
	flag.StringVar(&cfg.target, "target", "http://localhost:8080", "API gateway base URL")
	flag.DurationVar(&cfg.duration, "duration", 60*time.Second, "attack duration")
	flag.StringVar(&cfg.observerURL, "observer", "http://localhost:9090", "observer hub URL")
	flag.StringVar(&cfg.grafanaURL, "grafana", "http://localhost:3000", "Grafana base URL (M6 showcase page)")
	flag.IntVar(&cfg.pagePort, "port", 8090, "showcase page port (reserved for M6)")
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

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	attackCtx, cancelAttack := context.WithTimeout(context.Background(), cfg.duration)
	defer cancelAttack()

	results := runAttacks(attackCtx, keys, cfg, emitter, log)

	// Drain stop / sink errors briefly so the user can Ctrl-C mid-attack.
	select {
	case sig := <-stop:
		log.Info("interrupted", "signal", sig.String())
		cancelAttack()
	case err := <-sinkErr:
		return fmt.Errorf("sink server: %w", err)
	default:
	}

	printSummary(results, sink.counts(), log)

	shutCtx, shutCancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer shutCancel()
	if err := sinkSrv.Shutdown(shutCtx); err != nil {
		log.Warn("sink shutdown", "error", err)
	}
	return nil
}
