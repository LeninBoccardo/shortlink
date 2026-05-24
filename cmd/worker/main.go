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
	"github.com/leninboccardo/shortlink/internal/events"
	"github.com/leninboccardo/shortlink/internal/httpx"
	"github.com/leninboccardo/shortlink/internal/queue"
	"github.com/leninboccardo/shortlink/internal/security"
	"github.com/leninboccardo/shortlink/internal/storage"
	"github.com/leninboccardo/shortlink/internal/sweeper"
	"github.com/leninboccardo/shortlink/internal/webhook"
)

const (
	sweepInterval  = 60 * time.Second
	startupTimeout = 15 * time.Second
)

// worker holds the dependencies shared by the job handlers.
type worker struct {
	cfg        *config.Config
	log        *slog.Logger
	queries    *db.Queries
	store      *storage.ObjectStore
	queue      queue.Queue
	ssrf       *security.Validator
	dispatcher *webhook.Dispatcher
	emitter    *events.Emitter
	podID      string
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

	store, err := storage.NewObjectStore(startupCtx, cfg.MinioEndpoint, cfg.MinioAccessKey,
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

	emitter := events.NewEmitter(events.Config{
		URL:    cfg.ObserverURL,
		Token:  cfg.ObserverIngestToken,
		Source: events.SourceWorker,
		Logger: log,
	})

	podID := cfg.PodID
	if podID == "" {
		if h, err := os.Hostname(); err == nil {
			podID = h
		} else {
			podID = "worker"
		}
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
		emitter:    emitter,
		podID:      podID,
	}

	q.Register(queue.TypeShorten, w.handleShortenJob)
	q.Register(queue.TypeWebhook, w.handleWebhookJob)
	if err := q.Start(); err != nil {
		return err
	}

	// Pod heartbeat: refresh pod:{POD_ID}:alive (15s TTL) so the observer's
	// Redis poller can count live pods (SPEC §4.2/§4.3).
	heartbeatCtx, stopHeartbeat := context.WithCancel(context.Background())
	heartbeatDone := make(chan struct{})
	go func() {
		runHeartbeat(heartbeatCtx, rc, podID, log)
		close(heartbeatDone)
	}()
	emitter.Emit(events.Event{
		Level:   events.LevelInfo,
		Kind:    events.KindPodStarted,
		Message: "worker pod started",
		Meta:    map[string]any{"pod_id": podID},
	})
	log.Info("pod heartbeat started", "pod_id", podID, "ttl", heartbeatTTL)

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
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
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
	// 4. Emit pod_stopped, stop the heartbeat refresher (deletes the key),
	//    and flush the emitter.
	emitter.Emit(events.Event{
		Level:   events.LevelInfo,
		Kind:    events.KindPodStopped,
		Message: "worker pod stopped",
		Meta:    map[string]any{"pod_id": podID},
	})
	stopHeartbeat()
	<-heartbeatDone
	emitter.Close(2 * time.Second)

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
