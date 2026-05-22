// Command api is the ShortLink gateway. In Milestone 1 it also hosts the
// in-process job queue and worker pool — the worker becomes its own binary in
// M2 (SPEC §4.1).
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
	"github.com/leninboccardo/shortlink/internal/queue"
	"github.com/leninboccardo/shortlink/internal/security"
	"github.com/leninboccardo/shortlink/internal/storage"
	"github.com/leninboccardo/shortlink/internal/webhook"
)

const (
	queueBuffer     = 1024
	maxRequestBody  = 64 << 10 // 64 KiB cap on /shorten request bodies
	maxURLLength    = 2048
	httpDrainWindow = 10 * time.Second
)

// app holds the dependencies shared by the HTTP handlers and the in-process
// job handlers.
type app struct {
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

	ssrf := security.NewValidator(cfg.SSRFAllowlist)
	q := queue.NewInProc(cfg.WorkerConcurrency, queueBuffer, log)

	a := &app{
		cfg:        cfg,
		log:        log,
		queries:    db.New(pool),
		store:      store,
		queue:      q,
		ssrf:       ssrf,
		dispatcher: webhook.NewDispatcher(ssrf.SafeClient(webhook.DeliveryTimeout)),
	}

	q.Register(queue.TypeShorten, a.handleShortenJob)
	q.Register(queue.TypeWebhook, a.handleWebhookJob)
	q.Start()

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.APIPort),
		Handler:           a.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		log.Info("api gateway listening", "addr", srv.Addr)
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

	// 1. Stop accepting connections; let in-flight HTTP requests finish.
	httpCtx, cancel := context.WithTimeout(context.Background(), httpDrainWindow)
	defer cancel()
	if err := srv.Shutdown(httpCtx); err != nil {
		log.Error("http shutdown", "error", err)
	}
	// 2. Drain the in-process queue — buffered jobs run to completion.
	queueCtx, cancel2 := context.WithTimeout(context.Background(), cfg.DrainTimeout)
	defer cancel2()
	if err := q.Shutdown(queueCtx); err != nil {
		log.Error("queue drain", "error", err)
	}
	log.Info("shutdown complete")
	return nil
}
