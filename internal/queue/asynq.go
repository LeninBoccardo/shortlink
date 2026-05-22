package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hibiken/asynq"
)

// shortenMaxRetry caps shorten-job retries; the schedule below provides one
// delay per retry.
const shortenMaxRetry = 5

// webhookTaskTimeout bounds a single webhook task execution (a bounded POST).
const webhookTaskTimeout = 30 * time.Second

// Retry schedules (SPEC §7/§8): the delay before each subsequent attempt.
var (
	shortenRetrySchedule = []time.Duration{
		10 * time.Second, 30 * time.Second, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute,
	}
	webhookRetrySchedule = []time.Duration{
		5 * time.Second, 30 * time.Second, 2 * time.Minute, 5 * time.Minute,
	}
)

// AsynqConfig configures the Redis-backed queue.
type AsynqConfig struct {
	RedisURL           string
	Concurrency        int
	ShortenTimeout     time.Duration // crash-detection deadline; aligned with CLAIM_LEASE
	WebhookMaxAttempts int
	DrainTimeout       time.Duration
	Logger             *slog.Logger
}

// AsynqQueue is the Redis-backed Queue implementation (SPEC §7). One struct
// serves both roles: the gateway uses only Enqueue; the worker also Registers
// handlers and Starts the server.
type AsynqQueue struct {
	client *asynq.Client
	redis  asynq.RedisConnOpt
	cfg    AsynqConfig
	mux    *asynq.ServeMux
	srv    *asynq.Server
	log    *slog.Logger
}

// NewAsynqQueue builds a queue against the given Redis URL.
func NewAsynqQueue(cfg AsynqConfig) (*AsynqQueue, error) {
	redisOpt, err := asynq.ParseRedisURI(cfg.RedisURL)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	return &AsynqQueue{
		client: asynq.NewClient(redisOpt),
		redis:  redisOpt,
		cfg:    cfg,
		mux:    asynq.NewServeMux(),
		log:    cfg.Logger,
	}, nil
}

// Register binds a handler to a job type (and its same-named asynq queue).
func (q *AsynqQueue) Register(jobType string, h Handler) {
	q.mux.HandleFunc(jobType, func(ctx context.Context, t *asynq.Task) error {
		return h(ctx, t.Payload())
	})
}

// Enqueue submits a job. The job key becomes the asynq TaskID, so a repeated
// enqueue of the same job_id is deduplicated and reported as success.
func (q *AsynqQueue) Enqueue(ctx context.Context, job Job) error {
	opts := []asynq.Option{asynq.Queue(job.Type)}
	if job.Key != "" {
		opts = append(opts, asynq.TaskID(job.Key))
	}
	switch job.Type {
	case TypeShorten:
		opts = append(opts, asynq.MaxRetry(shortenMaxRetry), asynq.Timeout(q.cfg.ShortenTimeout))
	case TypeWebhook:
		opts = append(opts, asynq.MaxRetry(q.cfg.WebhookMaxAttempts-1), asynq.Timeout(webhookTaskTimeout))
	}
	_, err := q.client.EnqueueContext(ctx, asynq.NewTask(job.Type, job.Payload), opts...)
	if err != nil {
		if errors.Is(err, asynq.ErrTaskIDConflict) || errors.Is(err, asynq.ErrDuplicateTask) {
			return nil // already enqueued — idempotent success
		}
		return fmt.Errorf("enqueue %s: %w", job.Type, err)
	}
	return nil
}

// Start launches the asynq server with the registered handlers.
func (q *AsynqQueue) Start() error {
	q.srv = asynq.NewServer(q.redis, asynq.Config{
		Concurrency: q.cfg.Concurrency,
		Queues: map[string]int{
			TypeShorten: 3,
			TypeWebhook: 2,
		},
		RetryDelayFunc:  retryDelay,
		ShutdownTimeout: q.cfg.DrainTimeout,
		Logger:          asynqLogAdapter{q.log},
		LogLevel:        asynq.InfoLevel,
	})
	if err := q.srv.Start(q.mux); err != nil {
		return fmt.Errorf("start asynq server: %w", err)
	}
	q.log.Info("asynq queue started", "concurrency", q.cfg.Concurrency)
	return nil
}

// Shutdown drains in-flight tasks (bounded by ShutdownTimeout) and closes the
// client.
func (q *AsynqQueue) Shutdown(_ context.Context) error {
	if q.srv != nil {
		q.srv.Shutdown()
	}
	return q.client.Close()
}

// retryDelay returns the backoff before the next attempt, per job type.
func retryDelay(n int, _ error, task *asynq.Task) time.Duration {
	schedule := shortenRetrySchedule
	if task.Type() == TypeWebhook {
		schedule = webhookRetrySchedule
	}
	i := n - 1
	if i < 0 {
		i = 0
	}
	if i >= len(schedule) {
		i = len(schedule) - 1
	}
	return schedule[i]
}

// Attempt reports the 1-based current attempt number and the total attempt
// budget for the job being handled. Outside an asynq worker it returns (1, 1).
func Attempt(ctx context.Context) (current, total int) {
	retried, ok1 := asynq.GetRetryCount(ctx)
	maxRetry, ok2 := asynq.GetMaxRetry(ctx)
	if !ok1 || !ok2 {
		return 1, 1
	}
	return retried + 1, maxRetry + 1
}

// IsLastAttempt reports whether the job is on its final attempt — a returned
// error will send it to the dead-letter (archived) set.
func IsLastAttempt(ctx context.Context) bool {
	current, total := Attempt(ctx)
	return current >= total
}

// asynqLogAdapter routes asynq's internal logs through slog.
type asynqLogAdapter struct{ log *slog.Logger }

func (a asynqLogAdapter) Debug(args ...interface{}) { a.log.Debug(fmt.Sprint(args...)) }
func (a asynqLogAdapter) Info(args ...interface{})  { a.log.Info(fmt.Sprint(args...)) }
func (a asynqLogAdapter) Warn(args ...interface{})  { a.log.Warn(fmt.Sprint(args...)) }
func (a asynqLogAdapter) Error(args ...interface{}) { a.log.Error(fmt.Sprint(args...)) }
func (a asynqLogAdapter) Fatal(args ...interface{}) { a.log.Error(fmt.Sprint(args...)) }
