package queue

import (
	"context"
	"errors"
	"log/slog"
	"sync"
)

// ErrQueueClosed is returned by Enqueue after Shutdown has been called.
var ErrQueueClosed = errors.New("queue is closed")

// InProc is an in-process queue: a buffered channel drained by a fixed pool of
// worker goroutines. It has no durability — a hard crash loses whatever is
// still buffered. This is an accepted Milestone 1 limitation (SPEC §4.1/§7).
type InProc struct {
	jobs     chan Job
	handlers map[string]Handler
	workers  int
	log      *slog.Logger

	mu     sync.RWMutex // guards closed; serialises Enqueue sends against Shutdown
	closed bool
	wg     sync.WaitGroup
}

// NewInProc creates a queue with the given worker count and channel buffer.
func NewInProc(workers, buffer int, log *slog.Logger) *InProc {
	if workers < 1 {
		workers = 1
	}
	if buffer < 1 {
		buffer = 1
	}
	return &InProc{
		jobs:     make(chan Job, buffer),
		handlers: make(map[string]Handler),
		workers:  workers,
		log:      log,
	}
}

// Register binds a handler to a job type. Call before Start.
func (q *InProc) Register(jobType string, h Handler) {
	q.handlers[jobType] = h
}

// Start launches the worker pool.
func (q *InProc) Start() {
	for i := 0; i < q.workers; i++ {
		q.wg.Add(1)
		go q.worker()
	}
	q.log.Info("in-process queue started", "workers", q.workers)
}

// Enqueue submits a job. The RLock is held across the send so Shutdown — which
// takes the write lock — can never close the channel mid-send.
func (q *InProc) Enqueue(ctx context.Context, job Job) error {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.closed {
		return ErrQueueClosed
	}
	select {
	case q.jobs <- job:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (q *InProc) worker() {
	defer q.wg.Done()
	for job := range q.jobs {
		q.process(job)
	}
}

func (q *InProc) process(job Job) {
	defer func() {
		if r := recover(); r != nil {
			q.log.Error("job handler panicked", "type", job.Type, "panic", r)
		}
	}()
	h, ok := q.handlers[job.Type]
	if !ok {
		q.log.Error("no handler registered for job type", "type", job.Type)
		return
	}
	if err := h(context.Background(), job.Payload); err != nil {
		q.log.Error("job handler failed", "type", job.Type, "error", err)
	}
}

// Shutdown stops accepting new jobs, then drains everything already buffered
// before returning — or returns ctx.Err() if the drain outlasts ctx.
func (q *InProc) Shutdown(ctx context.Context) error {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return nil
	}
	q.closed = true
	close(q.jobs)
	q.mu.Unlock()

	done := make(chan struct{})
	go func() {
		q.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		q.log.Info("in-process queue drained")
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
