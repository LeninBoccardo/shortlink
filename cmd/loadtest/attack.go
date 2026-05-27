package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	vegeta "github.com/tsenart/vegeta/v12/lib"

	"github.com/leninboccardo/shortlink/internal/keysfile"
)

// seedURLs is the pool of original URLs each attacker randomises across.
// Real values are intentionally varied so the gateway's per-row inserts aren't
// fighting a slug-uniqueness hot-spot.
var seedURLs = []string{
	"https://example.com/blog",
	"https://example.com/about",
	"https://example.com/pricing",
	"https://example.com/docs/getting-started",
	"https://example.com/docs/api",
	"https://example.com/changelog",
	"https://example.com/careers",
	"https://example.com/contact",
	"https://example.com/legal/terms",
	"https://example.com/legal/privacy",
}

// attackResult is one key profile's outcome.
type attackResult struct {
	Profile  keysfile.Entry
	Metrics  vegeta.Metrics
	Started  time.Time
	Finished time.Time
}

// runAttacks fans one vegeta.Attacker out per key profile and waits for them
// all to finish (or for ctx to fire). Attackers are independent — one slow
// upstream tier doesn't hold up the others.
func runAttacks(ctx context.Context, keys *keysfile.File, cfg runConfig, log *slog.Logger) []attackResult {
	results := make([]attackResult, len(keys.Keys))
	var wg sync.WaitGroup
	for i, k := range keys.Keys {
		i, k := i, k
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = runOneProfile(ctx, k, cfg, log)
		}()
	}
	wg.Wait()
	return results
}

func runOneProfile(ctx context.Context, k keysfile.Entry, cfg runConfig, log *slog.Logger) attackResult {
	res := attackResult{Profile: k, Started: time.Now()}
	if k.AttackRatePerMin <= 0 {
		log.Warn("skip key with non-positive rate", "key", k.Name, "rate", k.AttackRatePerMin)
		res.Finished = time.Now()
		return res
	}
	rate := vegeta.Rate{Freq: k.AttackRatePerMin, Per: time.Minute}
	targeter := newTargeter(k, cfg)
	attacker := vegeta.NewAttacker(
		vegeta.Timeout(10*time.Second),
		vegeta.Workers(2),
		vegeta.MaxWorkers(8),
	)
	defer attacker.Stop()

	// Honour ctx by stopping the attack early if it cancels — vegeta's Attack
	// channel closes when the rate's duration elapses or attacker.Stop is called.
	stopOnCtx := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			attacker.Stop()
		case <-stopOnCtx:
		}
	}()

	var m vegeta.Metrics
	for r := range attacker.Attack(targeter, rate, cfg.duration, k.Name) {
		m.Add(r)
	}
	close(stopOnCtx)
	m.Close()
	res.Metrics = m
	res.Finished = time.Now()
	log.Info("attack profile done",
		"key", k.Name,
		"requests", m.Requests,
		"success_pct", fmt.Sprintf("%.1f", m.Success*100),
		"p99_ms", m.Latencies.P99.Milliseconds(),
	)
	return res
}

// newTargeter returns a vegeta targeter that builds a fresh POST /shorten on
// every call: random URL from seedURLs, fixed sink as webhook_url, the
// profile's X-Api-Key header.
func newTargeter(k keysfile.Entry, cfg runConfig) vegeta.Targeter {
	rng := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), uint64(time.Now().UnixNano()>>1)))
	return func(t *vegeta.Target) error {
		t.Method = http.MethodPost
		t.URL = cfg.target + "/shorten"
		body, _ := json.Marshal(map[string]string{
			"url":         seedURLs[rng.IntN(len(seedURLs))],
			"webhook_url": cfg.sinkURL,
		})
		t.Body = body
		t.Header = http.Header{
			"X-Api-Key":    []string{k.Key},
			"Content-Type": []string{"application/json"},
		}
		return nil
	}
}

