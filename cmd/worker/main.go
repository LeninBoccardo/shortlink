// Command worker processes the Redis-backed shorten and webhook queues and
// runs the sweeper. Split out of cmd/api in Milestone 2 (SPEC §4.2).
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
	"github.com/leninboccardo/shortlink/internal/db"
	"github.com/leninboccardo/shortlink/internal/httpx"
	"github.com/leninboccardo/shortlink/internal/queue"
	"github.com/leninboccardo/shortlink/internal/security"
	"github.com/leninboccardo/shortlink/internal/storage"
	"github.com/leninboccardo/shortlink/internal/sweeper"
	"github.com/leninboccardo/shortlink/internal/webhook"
)

const sweepInterval = 60 * time.Second

// worker holds the dependencies shared by the job handlers.
type worker struct {
	cfg        *config.Config
	log        *slog.Logger
	queries    *db.Queries
	store      *storage.ObjectStore
	queue      queue.Queue
	ssrf       *security.Validator
	dispatcher *webhook.Dispatcher
}

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

	ctx := context.Background()

	pool, err := storage.NewPool(ctx, cfg.DatabaseURL, cfg.PGPoolSize)
	if err != nil {
		return err
	}
	defer pool.Close()
	log.Info("connected to postgres")

	store, err := storage.NewObjectStore(ctx, cfg.MinioEndpoint, cfg.MinioAccessKey,
		cfg.MinioSecretKey, cfg.MinioBucket, cfg.MinioUseSSL)
	if err != nil {
		return err
	}
	log.Info("object storage ready", "bucket", cfg.MinioBucket)

	q, err := queue.NewAsynqQueue(queue.AsynqConfig{
		RedisURL:           cfg.RedisURL,
		Concurrency:        cfg.WorkerConcurrency,
		ShortenTimeout:     cfg.ClaimLease,
		WebhookMaxAttempts: cfg.WebhookMaxAttempts,
		DrainTimeout:       cfg.DrainTimeout,
		Logger:             log,
	})
	if err != nil {
		return err
	}

	ssrf := security.NewValidator(cfg.SSRFAllowlist)
	w := &worker{
		cfg:        cfg,
		log:        log,
		queries:    db.New(pool),
		store:      store,
		queue:      q,
		ssrf:       ssrf,
		dispatcher: webhook.NewDispatcher(ssrf.SafeClient(webhook.DeliveryTimeout)),
	}

	q.Register(queue.TypeShorten, w.handleShortenJob)
	q.Register(queue.TypeWebhook, w.handleWebhookJob)
	if err := q.Start(); err != nil {
		return err
	}

	// Sweeper loop.
	sweepCtx, stopSweeper := context.WithCancel(context.Background())
	sweeperDone := make(chan struct{})
	go func() {
		sweeper.New(w.queries, store, cfg.SweepStaleAge, cfg.QRObjectTTL, sweepInterval, log).Run(sweepCtx)
		close(sweeperDone)
	}()
	log.Info("sweeper started", "interval", sweepInterval)

	// Health endpoint.
	health := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.WorkerPort),
		Handler:           healthHandler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Info("worker health server listening", "addr", health.Addr)
		if err := health.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("health server", "error", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	sig := <-stop
	log.Info("shutdown signal received", "signal", sig.String())

	// 1. Stop the sweeper.
	stopSweeper()
	<-sweeperDone
	// 2. Drain in-flight jobs (asynq waits up to DrainTimeout).
	if err := q.Shutdown(context.Background()); err != nil {
		log.Error("queue shutdown", "error", err)
	}
	// 3. Stop the health server.
	healthCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = health.Shutdown(healthCtx)

	log.Info("shutdown complete")
	return nil
}

func healthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return mux
}
