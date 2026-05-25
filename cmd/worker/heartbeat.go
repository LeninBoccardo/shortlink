package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// heartbeatTTL is how long the observer treats a pod as alive after the last
// refresh. Set ~3× the refresh interval so a single skipped tick doesn't make
// the pod disappear from the active-pod count (SPEC §4.2/§4.3).
const (
	heartbeatTTL      = 15 * time.Second
	heartbeatInterval = 5 * time.Second
)

// runHeartbeat refreshes pod:{podID}:alive every heartbeatInterval until ctx
// is cancelled. On exit it deletes the key so the observer drops the pod from
// the live count immediately rather than waiting for the TTL.
func runHeartbeat(ctx context.Context, rc *redis.Client, podID string, log *slog.Logger) {
	key := "pod:" + podID + ":alive"
	refresh := func() {
		// Per-call timeout: a wedged Redis at SIGTERM mustn't hang shutdown.
		setCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if err := rc.Set(setCtx, key, "1", heartbeatTTL).Err(); err != nil && ctx.Err() == nil {
			log.Warn("pod heartbeat", "error", err)
		}
	}
	refresh() // first beat immediately so the observer sees us on startup
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			refresh()
		case <-ctx.Done():
			// Best-effort delete with a fresh short-lived context.
			delCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = rc.Del(delCtx, key).Err()
			cancel()
			return
		}
	}
}
