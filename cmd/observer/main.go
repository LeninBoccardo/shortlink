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

	"github.com/hibiken/asynq"

	"github.com/leninboccardo/shortlink/internal/config"
	"github.com/leninboccardo/shortlink/internal/observer"
	"github.com/leninboccardo/shortlink/internal/storage"
)

const (
	httpDrainWindow = 10 * time.Second
	shutdownTimeout = 5 * time.Second
	startupTimeout  = 15 * time.Second
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

	startupCtx, cancelStartup := context.WithTimeout(context.Background(), startupTimeout)
	defer cancelStartup()

	rc, err := storage.NewRedis(startupCtx, cfg.RedisURL)
	if err != nil {
		return err
	}
	defer rc.Close()
	log.Info("connected to redis")

	asynqOpt, err := asynq.ParseRedisURI(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("parse redis url for asynq inspector: %w", err)
	}
	inspector := asynq.NewInspector(asynqOpt)
	defer inspector.Close()

	hub := observer.NewHub(cfg.ObserverIngestToken, log)
	hub.Start()
	if cfg.ObserverIngestToken == "" {
		log.Warn("OBSERVER_INGEST_TOKEN unset — /ingest is open to any client (local-dev default)")
	}

	pollerCtx, stopPoller := context.WithCancel(context.Background())
	defer stopPoller()
	pollerDone := make(chan struct{})
	go func() {
		observer.NewPoller(hub, inspector, rc, cfg.QueueDepthThreshold, log).Run(pollerCtx)
		close(pollerDone)
	}()
	log.Info("redis poller started", "threshold", cfg.QueueDepthThreshold)

	broadcaster := observer.NewBroadcaster(hub, log)
	mux := hub.Routes()
	broadcaster.Register(mux)
	broadcaster.Start()
	log.Info("websocket broadcaster started")

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.ObserverPort),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		// /stream is a long-lived WebSocket — no overall ReadTimeout/WriteTimeout
		// or the upgrader would kill it. Browsers stay connected, per-message
		// deadlines are set inside the broadcaster.
		IdleTimeout: 60 * time.Second,
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
	// 2. Stop the WebSocket broadcaster (closes all client connections).
	bcastCtx, bcastCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	if err := broadcaster.Shutdown(bcastCtx); err != nil {
		log.Error("broadcaster shutdown", "error", err)
	}
	bcastCancel()
	// 3. Stop the Redis poller.
	stopPoller()
	<-pollerDone
	// 4. Drain the aggregator.
	hubCtx, hubCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer hubCancel()
	if err := hub.Shutdown(hubCtx); err != nil {
		log.Error("hub shutdown", "error", err)
	}
	log.Info("shutdown complete")
	return nil
}
