// Package config loads runtime configuration from environment variables.
//
// Fields are introduced milestone by milestone (SPEC §14). This covers
// Milestones 1–2: HTTP, Postgres, Redis, object storage, shortening/QR, the
// worker/queue, the sweeper, and SSRF/URL security.
package config

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

// Config is the fully-resolved configuration shared by the ShortLink binaries.
// Each binary reads only the fields it needs.
type Config struct {
	LogLevel string `env:"LOG_LEVEL" envDefault:"info"`

	// HTTP
	APIPort      int    `env:"API_PORT" envDefault:"8080"`
	WorkerPort   int    `env:"WORKER_PORT" envDefault:"8081"`
	ShortURLBase string `env:"SHORT_URL_BASE" envDefault:"http://localhost:8080"`

	// Postgres
	DatabaseURL string `env:"DATABASE_URL" envDefault:"postgres://shortlink:shortlink@localhost:16432/shortlink?sslmode=disable"`
	// API_PG_POOL_SIZE / WORKER_PG_POOL_SIZE are per-binary so each pool
	// can be sized to its actual concurrency (SPEC §14). The api keeps a
	// larger pool because every incoming request runs auth + insert (two
	// queries) and replicas are bounded; workers run a small fixed number
	// of goroutines per pod and the worker tier scales horizontally, so
	// holding 8 connections per pod would explode the total connection
	// count at the upstream Postgres.
	APIPGPoolSize    int32 `env:"API_PG_POOL_SIZE" envDefault:"8"`
	WorkerPGPoolSize int32 `env:"WORKER_PG_POOL_SIZE" envDefault:"4"`

	// Redis — backs the asynq task queue
	RedisURL string `env:"REDIS_URL" envDefault:"redis://localhost:6379"`

	// Object storage (MinIO locally, any S3-compatible store in production)
	MinioEndpoint  string        `env:"MINIO_ENDPOINT" envDefault:"localhost:9000"`
	MinioAccessKey string        `env:"MINIO_ACCESS_KEY" envDefault:"minioadmin"`
	MinioSecretKey string        `env:"MINIO_SECRET_KEY" envDefault:"minioadmin"`
	MinioBucket    string        `env:"MINIO_BUCKET" envDefault:"shortlink-qr"`
	MinioUseSSL    bool          `env:"MINIO_USE_SSL" envDefault:"false"`
	SignedURLTTL   time.Duration `env:"SIGNED_URL_TTL" envDefault:"60s"`

	// Shortening and QR generation
	QRSize         int `env:"QR_SIZE" envDefault:"256"`
	SlugLength     int `env:"SLUG_LENGTH" envDefault:"7"`
	SlugMaxRetries int `env:"SLUG_MAX_RETRIES" envDefault:"5"`

	// Worker / task queue
	WorkerConcurrency  int           `env:"WORKER_CONCURRENCY" envDefault:"3"`
	ClaimLease         time.Duration `env:"CLAIM_LEASE" envDefault:"2m"`
	WebhookMaxAttempts int           `env:"WEBHOOK_MAX_ATTEMPTS" envDefault:"5"`
	DrainTimeout       time.Duration `env:"DRAIN_TIMEOUT" envDefault:"30s"`

	// Sweeper
	SweepStaleAge time.Duration `env:"SWEEP_STALE_AGE" envDefault:"30m"`
	QRObjectTTL   time.Duration `env:"QR_OBJECT_TTL" envDefault:"15m"`

	// Rate limiting (per-key sliding 60s window — SPEC §4.1/§9)
	RateLimitFree    int           `env:"RATE_LIMIT_FREE" envDefault:"10"`
	RateLimitPro     int           `env:"RATE_LIMIT_PRO" envDefault:"60"`
	LastUsedThrottle time.Duration `env:"LAST_USED_THROTTLE" envDefault:"5m"`

	// Observer hub (SPEC §4.3/§10) — every service emits best-effort events
	// here; if it's down, events are dropped without blocking the caller.
	// Local default is 9090, not 9000, because MinIO's local docker-compose
	// publishes its S3 API on host 9000 — the two would collide otherwise.
	ObserverURL         string `env:"OBSERVER_URL" envDefault:"http://localhost:9090"`
	ObserverPort        int    `env:"OBSERVER_PORT" envDefault:"9090"`
	QueueDepthThreshold int64  `env:"QUEUE_DEPTH_THRESHOLD" envDefault:"100"`
	PodID               string `env:"POD_ID"`
	// ObserverIngestToken gates POST /ingest. Empty (the local-dev default)
	// keeps /ingest open. When set, the observer requires
	// `Authorization: Bearer <token>` and every emitter (api/worker/loadtest)
	// must be started with the same value so it can attach the header.
	ObserverIngestToken string `env:"OBSERVER_INGEST_TOKEN"`
	// ObserverAllowedOrigins is the comma-separated list of page origins the
	// WebSocket upgrader trusts (cross-origin connect from the showcase page).
	// Local default permits the loadtest showcase on :8090 over loopback.
	ObserverAllowedOrigins []string `env:"OBSERVER_ALLOWED_ORIGINS" envSeparator:"," envDefault:"http://localhost:8090,http://127.0.0.1:8090"`

	// Security
	SSRFAllowlist []string `env:"SSRF_ALLOWLIST" envSeparator:","`
	URLBlocklist  []string `env:"URL_BLOCKLIST" envSeparator:","`
}

// Load parses the environment into a Config, applying the defaults above.
func Load() (*Config, error) {
	cfg, err := env.ParseAs[Config]()
	if err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return &cfg, nil
}

// SlogLevel maps the configured LOG_LEVEL string onto a slog.Level,
// defaulting to info for any unrecognised value.
func (c *Config) SlogLevel() slog.Level {
	switch strings.ToLower(c.LogLevel) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
