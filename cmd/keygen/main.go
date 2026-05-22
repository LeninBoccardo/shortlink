// Command keygen provisions test API keys: it generates raw keys + webhook
// secrets, inserts their hashes into Postgres, and writes the raw material to
// config/keys.yaml for the load-test runner (SPEC §4.4/§13).
//
// keys.yaml contains real secrets and is gitignored. Re-running keygen appends
// three more keys and overwrites keys.yaml.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/jackc/pgx/v5/pgtype"
	"gopkg.in/yaml.v3"

	"github.com/leninboccardo/shortlink/internal/auth"
	"github.com/leninboccardo/shortlink/internal/config"
	"github.com/leninboccardo/shortlink/internal/db"
	"github.com/leninboccardo/shortlink/internal/storage"
)

const keysPath = "config/keys.yaml"

// profiles are the three load-test key tiers from SPEC §4.4.
var profiles = []struct {
	name string
	tier string
	rate int
}{
	{name: "Free tier", tier: "free", rate: 10},
	{name: "Pro tier", tier: "pro", rate: 60},
	{name: "Abuser (over-limit)", tier: "pro", rate: 200},
}

type keyEntry struct {
	Name             string `yaml:"name"`
	Key              string `yaml:"key"`
	WebhookSecret    string `yaml:"webhook_secret"`
	AttackRatePerMin int    `yaml:"attack_rate_per_min"`
	Tier             string `yaml:"tier"`
}

type keysFile struct {
	Keys []keyEntry `yaml:"keys"`
}

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	ctx := context.Background()
	pool, err := storage.NewPool(ctx, cfg.DatabaseURL, 2)
	if err != nil {
		log.Fatalf("postgres: %v", err)
	}
	defer pool.Close()
	queries := db.New(pool)

	var out keysFile
	fmt.Println("Generated API keys (shown once — store them now):")
	for _, p := range profiles {
		raw, err := auth.NewAPIKey()
		if err != nil {
			log.Fatalf("generate key: %v", err)
		}
		secret, err := auth.NewWebhookSecret()
		if err != nil {
			log.Fatalf("generate webhook secret: %v", err)
		}
		if _, err := queries.CreateAPIKey(ctx, db.CreateAPIKeyParams{
			KeyHash:       auth.HashKey(raw),
			KeyHint:       auth.Hint(raw),
			Name:          p.name,
			Tier:          p.tier,
			WebhookSecret: secret,
			WebhookUrl:    pgtype.Text{Valid: false},
		}); err != nil {
			log.Fatalf("insert key %q: %v", p.name, err)
		}
		out.Keys = append(out.Keys, keyEntry{
			Name:             p.name,
			Key:              raw,
			WebhookSecret:    secret,
			AttackRatePerMin: p.rate,
			Tier:             p.tier,
		})
		fmt.Printf("  %-22s %s  (tier=%s)\n", p.name, raw, p.tier)
	}

	if err := os.MkdirAll(filepath.Dir(keysPath), 0o755); err != nil {
		log.Fatalf("create config dir: %v", err)
	}
	data, err := yaml.Marshal(out)
	if err != nil {
		log.Fatalf("marshal keys: %v", err)
	}
	if err := os.WriteFile(keysPath, data, 0o600); err != nil {
		log.Fatalf("write %s: %v", keysPath, err)
	}
	fmt.Printf("\nWrote %d keys to %s (gitignored — contains real secrets).\n", len(out.Keys), keysPath)
}
