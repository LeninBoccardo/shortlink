// Package queue decouples job submission from execution. Milestone 1 shipped an
// in-process channel implementation; from Milestone 2 the Redis-backed asynq
// implementation is used — both behind this same interface (SPEC §7).
package queue

import "context"

// Job is a unit of work: a type tag, a stable key for enqueue-deduplication,
// and a JSON-encoded payload.
type Job struct {
	Type    string // job type; also the asynq queue name
	Key     string // stable identifier (the job_id) — deduplicates re-enqueues
	Payload []byte
}

// Handler processes one job payload. A non-nil error marks the attempt failed;
// the asynq queue then retries with backoff or archives to the dead-letter set.
type Handler func(ctx context.Context, payload []byte) error

// Queue accepts jobs and dispatches them to registered handlers.
type Queue interface {
	// Register binds a handler to a job type. Call before Start.
	Register(jobType string, h Handler)
	// Enqueue submits a job for asynchronous processing.
	Enqueue(ctx context.Context, job Job) error
	// Start launches the worker side of the queue.
	Start() error
	// Shutdown stops intake and drains outstanding work.
	Shutdown(ctx context.Context) error
}
