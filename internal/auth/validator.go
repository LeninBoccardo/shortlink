package auth

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/leninboccardo/shortlink/internal/db"
	"github.com/leninboccardo/shortlink/internal/metrics"
)

// ErrInvalidKey is returned when an API key is missing, unknown, or revoked.
var ErrInvalidKey = errors.New("invalid api key")

// validatorCacheTTL is the per-entry lifetime for the in-process key cache.
// Bounds the staleness of a revoked key on this process; a revocation visible
// in Postgres takes up to this long to propagate.
const validatorCacheTTL = 60 * time.Second

// validatorCacheSweepInterval controls how often Run walks the cache to
// delete entries whose TTL has expired. The TTL itself only gates *use* of
// an entry; without a sweeper the cache grows monotonically as new key
// hashes accumulate, which at long-running-process scale is a slow leak.
// 5 minutes is far larger than the TTL so the per-tick scan amortises
// cheaply (most entries are either fresh-and-staying-fresh or expired-
// and-about-to-be-deleted; the in-between window is brief).
const validatorCacheSweepInterval = 5 * time.Minute

// Validator authenticates raw API keys against the api_keys table. Successful
// lookups are cached for validatorCacheTTL so the redirect / shorten hot path
// doesn't hit Postgres on every request. Failed lookups are NOT cached, so a
// brute-force attempt can't flood the cache with junk entries.
type Validator struct {
	q     *db.Queries
	cache sync.Map // map[string]*validatorCacheEntry, key = HashKey(raw)
}

type validatorCacheEntry struct {
	key       db.ApiKey
	expiresAt time.Time
}

// NewValidator builds a Validator over the given query set.
func NewValidator(q *db.Queries) *Validator {
	return &Validator{q: q}
}

// Validate hashes raw and looks it up. A missing or revoked key — both yield
// pgx.ErrNoRows from the query — is reported as ErrInvalidKey.
//
// Malformed keys are rejected by ValidKeyFormat before any DB or cache work,
// so invalid-key spam never reaches Postgres (failed lookups are deliberately
// uncached to prevent brute-force pollution).
func (v *Validator) Validate(ctx context.Context, raw string) (db.ApiKey, error) {
	if !ValidKeyFormat(raw) {
		return db.ApiKey{}, ErrInvalidKey
	}
	hash := HashKey(raw)
	if e, ok := v.cache.Load(hash); ok {
		entry := e.(*validatorCacheEntry)
		if time.Now().Before(entry.expiresAt) {
			metrics.AuthKeyCacheTotal.WithLabelValues("hit").Inc()
			return entry.key, nil
		}
	}
	metrics.AuthKeyCacheTotal.WithLabelValues("miss").Inc()
	key, err := v.q.GetAPIKeyByHash(ctx, hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.ApiKey{}, ErrInvalidKey
		}
		return db.ApiKey{}, err
	}
	v.cache.Store(hash, &validatorCacheEntry{
		key:       key,
		expiresAt: time.Now().Add(validatorCacheTTL),
	})
	return key, nil
}

// Run blocks until ctx is canceled, periodically deleting cache entries
// whose TTL has passed. The Validate path only reads the cache (and rejects
// stale entries on read), so without this sweeper a long-running api
// process accumulates one *validatorCacheEntry per distinct key hash it
// ever sees, forever. At expected operator scale that's nothing; at "this
// process has been validating a million unique keys over weeks" it's a
// slow memory leak.
//
// Caller spawns this as a goroutine; cancel ctx to stop. Safe to skip
// calling — Validate works fine without the sweeper, just at the cost of
// unbounded cache growth.
func (v *Validator) Run(ctx context.Context) {
	ticker := time.NewTicker(validatorCacheSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			v.sweepExpired(now)
		}
	}
}

// sweepExpired walks the cache once and deletes any entry whose expiresAt
// is in the past (relative to the passed-in now). sync.Map.Range is safe
// to interleave with Delete on the same key.
func (v *Validator) sweepExpired(now time.Time) {
	v.cache.Range(func(key, val any) bool {
		entry := val.(*validatorCacheEntry)
		if !now.Before(entry.expiresAt) {
			v.cache.Delete(key)
		}
		return true
	})
}
