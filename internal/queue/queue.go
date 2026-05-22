// Package queue decouples job submission from execution. Milestone 1 ships an
// in-process channel implementation; M2 swaps in a Redis/asynq one behind this
// same interface without changing the API contract (SPEC §7).
package queue

import "context"

// Job is a unit of work: a type tag plus a JSON-encoded payload.
type Job struct {
	Type    string
	Payload []byte
}

// Handler processes one job payload. A returned error is logged; the
// in-process queue does not retry — retry and the dead-letter queue arrive
// with Redis in M2.
type Handler func(ctx context.Context, payload []byte) error

// Queue accepts jobs and dispatches them to registered handlers.
type Queue interface {
	// Register binds a handler to a job type. Call before Start.
	Register(jobType string, h Handler)
	// Enqueue submits a job for asynchronous processing.
	Enqueue(ctx context.Context, job Job) error
	// Start launches the worker pool.
	Start()
	// Shutdown stops intake and drains outstanding work, or returns when ctx
	// expires.
	Shutdown(ctx context.Context) error
}
