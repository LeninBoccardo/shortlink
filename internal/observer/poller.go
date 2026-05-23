package observer

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"github.com/leninboccardo/shortlink/internal/events"
	"github.com/leninboccardo/shortlink/internal/queue"
)

// pollInterval is how often the poller queries Redis (SPEC §4.3).
const pollInterval = 5 * time.Second

// pollerQueues is the set of asynq queues whose pending/archived sizes
// contribute to the observer's QueueDepth / dlq_nonempty signals.
var pollerQueues = []string{queue.TypeShorten, queue.TypeWebhook}

// Poller derives system-wide stats from Redis: total queue depth across the
// asynq queues, archived (dead-letter) size, and live pod count from
// pod:{POD_ID}:alive keys. It writes results into the Hub's State and
// enqueues queue_depth_high / dlq_nonempty events when thresholds trip.
type Poller struct {
	hub        *Hub
	inspector  *asynq.Inspector
	rc         *redis.Client
	threshold  int64
	log        *slog.Logger
	lastHighOK bool // last tick's "queue depth high" state, for edge detection
	lastDLQOK  bool
}

// NewPoller wires a poller. inspector reads asynq queue metadata; rc is a
// general-purpose go-redis client used for the pod-heartbeat SCAN.
func NewPoller(hub *Hub, inspector *asynq.Inspector, rc *redis.Client, threshold int64, log *slog.Logger) *Poller {
	return &Poller{
		hub:       hub,
		inspector: inspector,
		rc:        rc,
		threshold: threshold,
		log:       log,
	}
}

// Run polls until ctx is cancelled. Failures are logged at debug — a Redis
// blip should never crash the observer.
func (p *Poller) Run(ctx context.Context) {
	// Tick once immediately so the dashboard isn't empty for 5s on startup.
	p.tick(ctx)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			p.tick(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (p *Poller) tick(ctx context.Context) {
	var totalPending int64
	var totalArchived int64
	for _, name := range pollerQueues {
		info, err := p.inspector.GetQueueInfo(name)
		if err != nil {
			// Queue may not exist yet (no jobs ever enqueued) — that's fine.
			p.log.Debug("inspect queue", "queue", name, "error", err)
			continue
		}
		totalPending += int64(info.Pending)
		totalArchived += int64(info.Archived)
	}
	p.hub.State().SetQueueDepth(totalPending)

	pods, err := p.countAlivePods(ctx)
	if err != nil {
		p.log.Debug("count alive pods", "error", err)
	} else {
		p.hub.State().SetPodCount(pods)
	}

	// Edge-emit: only fire queue_depth_high on the tick we cross the threshold,
	// not every 5s while we're above it. Same idea for dlq_nonempty.
	highNow := totalPending > p.threshold
	if highNow && !p.lastHighOK {
		p.hub.Enqueue(events.Event{
			Source:  events.SourceObserver,
			Level:   events.LevelWarn,
			Kind:    events.KindQueueDepthHigh,
			Message: fmt.Sprintf("queue depth %d exceeds threshold %d", totalPending, p.threshold),
			Meta: map[string]any{
				"pending":   totalPending,
				"threshold": p.threshold,
			},
		})
	}
	p.lastHighOK = highNow

	dlqNow := totalArchived > 0
	if dlqNow && !p.lastDLQOK {
		p.hub.Enqueue(events.Event{
			Source:  events.SourceObserver,
			Level:   events.LevelWarn,
			Kind:    events.KindDLQNonempty,
			Message: fmt.Sprintf("dead-letter queue holds %d archived jobs", totalArchived),
			Meta: map[string]any{
				"archived": totalArchived,
			},
		})
	}
	p.lastDLQOK = dlqNow
}

// countAlivePods counts pod:*:alive keys via SCAN — avoid KEYS, which is
// O(N) blocking. The set is tiny (one per worker pod) so a single SCAN pass
// with a generous Count is plenty.
func (p *Poller) countAlivePods(ctx context.Context) (int, error) {
	var count int
	iter := p.rc.Scan(ctx, 0, "pod:*:alive", 100).Iterator()
	for iter.Next(ctx) {
		count++
	}
	if err := iter.Err(); err != nil {
		return 0, err
	}
	return count, nil
}
