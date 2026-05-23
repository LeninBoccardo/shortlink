package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// rateLimitScript is the per-key sliding-window check (SPEC §9). Atomic via
// Lua: prune expired entries, count, and add the current request iff under
// the limit. Returns {allowed (0/1), count_after, oldest_score_ms}.
const rateLimitScript = `
local key = KEYS[1]
local now = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local limit = tonumber(ARGV[3])
local member = ARGV[4]
local cutoff = now - window

redis.call('ZREMRANGEBYSCORE', key, '-inf', '(' .. cutoff)
local count = redis.call('ZCARD', key)
local allowed = 0
if count < limit then
    redis.call('ZADD', key, now, member)
    redis.call('PEXPIRE', key, window)
    allowed = 1
    count = count + 1
end

local oldest = 0
local arr = redis.call('ZRANGE', key, 0, 0, 'WITHSCORES')
if #arr >= 2 then
    oldest = tonumber(arr[2])
end

return {allowed, count, oldest}
`

// RateLimiter enforces a per-key sliding-window rate limit in Redis.
type RateLimiter struct {
	client *redis.Client
	window time.Duration
	script *redis.Script
}

// Decision reports the outcome of a rate-limit check; the gateway echoes Limit,
// Remaining, and ResetAt back in X-RateLimit-* headers.
type Decision struct {
	Allowed   bool
	Limit     int // 0 means unlimited — no rate limit was applied
	Remaining int
	ResetAt   time.Time // when the next slot frees up
}

// NewRateLimiter builds a limiter over client with the given window length.
func NewRateLimiter(client *redis.Client, window time.Duration) *RateLimiter {
	return &RateLimiter{
		client: client,
		window: window,
		script: redis.NewScript(rateLimitScript),
	}
}

// Check consumes one slot for keyHash. limit <= 0 is the unlimited tier and is
// always allowed without touching Redis. requestID must be unique per request
// (use the chi RequestID) — it becomes the sorted-set member.
func (r *RateLimiter) Check(ctx context.Context, keyHash string, limit int, requestID string) (Decision, error) {
	if limit <= 0 {
		return Decision{Allowed: true}, nil
	}
	now := time.Now()
	res, err := r.script.Run(ctx, r.client, []string{"rl:" + keyHash},
		now.UnixMilli(), r.window.Milliseconds(), limit, requestID).Slice()
	if err != nil {
		return Decision{}, fmt.Errorf("rate limit eval: %w", err)
	}
	if len(res) != 3 {
		return Decision{}, fmt.Errorf("rate limit eval: unexpected result %v", res)
	}
	allowed, _ := res[0].(int64)
	count, _ := res[1].(int64)
	oldest, _ := res[2].(int64)

	remaining := limit - int(count)
	if remaining < 0 {
		remaining = 0
	}
	resetAt := now.Add(r.window)
	if oldest > 0 {
		resetAt = time.UnixMilli(oldest).Add(r.window)
	}
	return Decision{
		Allowed:   allowed == 1,
		Limit:     limit,
		Remaining: remaining,
		ResetAt:   resetAt,
	}, nil
}
