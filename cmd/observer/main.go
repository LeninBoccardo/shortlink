// Command observer is the ShortLink observer hub (SPEC §4.3): it ingests
// structured events from the api gateway and worker, aggregates them in
// memory with TTL, and (in the next commits) polls Redis for system stats
// and broadcasts everything over WebSocket to the showcase page.
//
// Backend only — it serves no static files. The frontend lives in the load
// test runner (cmd/loadtest, M6).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/leninboccardo/shortlink/internal/config"
	"github.com/leninboccardo/shortlink/internal/observer"
)

const (
	httpDrainWindow = 10 * time.Second
	shutdownTimeout = 5 * time.Second
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.SlogLevel()}))
	slog.SetDefault(log)

	hub := observer.NewHub(log)
	hub.Start()

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.ObserverPort),
		Handler:           hub.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		log.Info("observer hub listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		return fmt.Errorf("http server: %w", err)
	case sig := <-stop:
		log.Info("shutdown signal received", "signal", sig.String())
	}

	// 1. Stop accepting connections; let in-flight requests finish.
	httpCtx, cancel := context.WithTimeout(context.Background(), httpDrainWindow)
	defer cancel()
	if err := srv.Shutdown(httpCtx); err != nil {
		log.Error("http shutdown", "error", err)
	}
	// 2. Drain the aggregator.
	hubCtx, hubCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer hubCancel()
	if err := hub.Shutdown(hubCtx); err != nil {
		log.Error("hub shutdown", "error", err)
	}
	log.Info("shutdown complete")
	return nil
}
