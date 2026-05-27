// Command keygen provisions test API keys: it generates raw keys + webhook
// secrets, inserts their hashes into Postgres, and writes the raw material to
// config/keys.yaml for the load-test runner (SPEC §4.4/§13).
//
// keys.yaml contains real secrets and is gitignored. By default re-running
// keygen inserts three more keys (the prior batch stays valid in Postgres
// even though keys.yaml on disk is overwritten). Pass --replace to revoke
// every still-active key first, so the new keys.yaml is the only thing that
// authenticates. --replace shows the target DATABASE_URL host + active-key
// count and waits for a "yes" before running; pass --yes to skip the prompt
// (required in non-interactive shells / CI).
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/leninboccardo/shortlink/internal/auth"
	"github.com/leninboccardo/shortlink/internal/config"
	"github.com/leninboccardo/shortlink/internal/db"
	"github.com/leninboccardo/shortlink/internal/keysfile"
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

func main() {
	replace := flag.Bool("replace", false, "revoke every still-active key in api_keys before inserting the new batch")
	yes := flag.Bool("yes", false, "skip the --replace confirmation prompt (required for non-interactive shells / CI)")
	flag.Parse()

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

	if *replace {
		active, err := queries.CountActiveAPIKeys(ctx)
		if err != nil {
			log.Fatalf("count active keys: %v", err)
		}
		host := dsnHost(cfg.DatabaseURL)
		// Echo what's about to happen BEFORE the destructive call. If the
		// operator has DATABASE_URL pointed at the wrong cluster (a stale
		// shell export, a forgotten dev/prod toggle), the host string + the
		// active-key count are the load-bearing signals that this is not
		// the database they meant to nuke.
		fmt.Printf("--replace will revoke %d active API key(s) in DATABASE_URL host %q.\n", active, host)
		if !*yes {
			fmt.Print("Type 'yes' to continue (anything else aborts): ")
			reader := bufio.NewReader(os.Stdin)
			line, _ := reader.ReadString('\n')
			if strings.TrimSpace(line) != "yes" {
				log.Fatal("aborted by user (no keys revoked, no keys created)")
			}
		}
		n, err := queries.RevokeAllActiveAPIKeys(ctx)
		if err != nil {
			log.Fatalf("revoke active keys: %v", err)
		}
		fmt.Printf("Revoked %d active key(s) before inserting new batch.\n", n)
	}

	var out keysfile.File
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
		out.Keys = append(out.Keys, keysfile.Entry{
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
	if err := keysfile.Write(keysPath, &out); err != nil {
		log.Fatalf("write keys file: %v", err)
	}
	fmt.Printf("\nWrote %d keys to %s (gitignored — contains real secrets).\n", len(out.Keys), keysPath)
}

// dsnHost extracts the host[:port] portion of a Postgres DSN for display.
// Returns a sentinel string rather than the raw DSN on parse failure — the
// DSN may carry an inline password we shouldn't echo to the terminal.
func dsnHost(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || u.Host == "" {
		return "<unparseable DSN>"
	}
	return u.Host
}
