// Command api is the ShortLink gateway: it authenticates requests, applies the
// per-key rate limit, reserves a short_urls row, and enqueues a shorten job
// onto the Redis-backed queue. From Milestone 2 job processing lives in the
// separate cmd/worker binary.
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

	"github.com/leninboccardo/shortlink/internal/auth"
	"github.com/leninboccardo/shortlink/internal/config"
	"github.com/leninboccardo/shortlink/internal/db"
	"github.com/leninboccardo/shortlink/internal/events"
	"github.com/leninboccardo/shortlink/internal/queue"
	"github.com/leninboccardo/shortlink/internal/security"
	"github.com/leninboccardo/shortlink/internal/storage"
)

const (
	maxRequestBody = 64 << 10                // 64 KiB cap on /shorten request bodies
	maxURLLength   = 2048                    // SPEC §9
	maxExpiresIn   = 10 * 365 * 24 * 60 * 60 // 10 years, in seconds

	// maxReturnsAfter is the upper bound (seconds) for item 8's
	// `returns_after` field. Capped just below the default QR_OBJECT_TTL
	// (15m) so the QR object the worker presigns at delivery time still
	// exists in MinIO. Raising QR_OBJECT_TTL above 15m doesn't raise this
	// cap — that takes a code change so the safety margin stays explicit.
	maxReturnsAfter = 14 * 60 // 14 minutes

	httpDrainWindow = 10 * time.Second
	startupTimeout  = 15 * time.Second
	rateLimitWindow = time.Minute // SPEC §4.1 — fixed 60s sliding window
)

// app holds the dependencies shared by the gateway's HTTP handlers.
type app struct {
	cfg       *config.Config
	log       *slog.Logger
	queries   *db.Queries
	queue     queue.Queue
	ssrf      *security.Validator
	hits      *hitRecorder
	validator *auth.Validator
	toucher   *auth.LastUsedToucher
	limiter   *auth.RateLimiter
	emitter   *events.Emitter
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

	startupCtx, cancelStartup := context.WithTimeout(context.Background(), startupTimeout)
	defer cancelStartup()

	pool, err := storage.NewPool(startupCtx, cfg.DatabaseURL, cfg.PGPoolSize)
	if err != nil {
		return err
	}
	defer pool.Close()
	log.Info("connected to postgres")

	rc, err := storage.NewRedis(startupCtx, cfg.RedisURL)
	if err != nil {
		return err
	}
	defer rc.Close()
	log.Info("connected to redis")

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
	log.Info("redis queue client ready")

	emitter := events.NewEmitter(events.Config{
		URL:    cfg.ObserverURL,
		Token:  cfg.ObserverIngestToken,
		Source: events.SourceAPI,
		Logger: log,
	})

	queries := db.New(pool)
	a := &app{
		cfg:       cfg,
		log:       log,
		queries:   queries,
		queue:     q,
		ssrf:      security.NewValidator(cfg.SSRFAllowlist),
		hits:      newHitRecorder(queries, log),
		validator: auth.NewValidator(queries),
		toucher:   auth.NewLastUsedToucher(queries, rc, cfg.LastUsedThrottle, log),
		limiter:   auth.NewRateLimiter(rc, rateLimitWindow),
		emitter:   emitter,
	}

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.APIPort),
		Handler:           a.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
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

	// 1. Stop accepting connections; let in-flight requests finish.
	httpCtx, cancel := context.WithTimeout(context.Background(), httpDrainWindow)
	defer cancel()
	if err := srv.Shutdown(httpCtx); err != nil {
		log.Error("http shutdown", "error", err)
	}
	// 2. Drain buffered analytics writes — safe now that no handler is running.
	a.hits.shutdown()
	// 3. Close the queue client.
	if err := q.Shutdown(context.Background()); err != nil {
		log.Error("queue shutdown", "error", err)
	}
	// 4. Flush any pending observer events with a bounded grace.
	a.emitter.Close(2 * time.Second)
	log.Info("shutdown complete")
	return nil
}
