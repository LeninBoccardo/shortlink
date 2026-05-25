// Command limits is the central tool for ShortLink's local resource budget.
//
// It detects the host's CPU + RAM, reads config/local-limits.yaml, validates
// the totals fit, and renders overlay files for both docker compose
// (deploy/docker-compose.override.yml) and the Helm chart
// (deploy/k8s/values-local.yaml). Both setup scripts call `limits render`
// before bringing the stack up, so per-machine sizing is one edit away.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
)

const usage = `cmd/limits  central tool for ShortLink's local resource budget

Subcommands:
  detect [--json]   Print host CPU/RAM capacity.
  validate          Read the config, check the totals fit; exit 0 OK,
                    1 over-budget, 2 config error.
  render            Validate then write deploy/docker-compose.override.yml
                    and deploy/k8s/values-local.yaml from the config.
  help              This message.

Global flags:
  --config <path>   Path to local-limits.yaml. Default: config/local-limits.yaml.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd := os.Args[1]

	fs := flag.NewFlagSet("limits", flag.ExitOnError)
	configPath := fs.String("config", "config/local-limits.yaml", "path to limits config")
	jsonOut := fs.Bool("json", false, "JSON output (detect only)")
	if err := fs.Parse(os.Args[2:]); err != nil {
		os.Exit(2)
	}

	switch cmd {
	case "detect":
		runDetect(*jsonOut)
	case "validate":
		runValidate(*configPath)
	case "render":
		runRender(*configPath)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n\n%s", cmd, usage)
		os.Exit(2)
	}
}

func runDetect(asJSON bool) {
	h, err := DetectHost()
	if err != nil {
		fmt.Fprintf(os.Stderr, "detect: %v\n", err)
		os.Exit(1)
	}
	if asJSON {
		out, _ := json.Marshal(h)
		fmt.Println(string(out))
		return
	}
	fmt.Printf("CPUs (logical):  %d\n", h.LogicalCPUs)
	fmt.Printf("Memory (total):  %d MB (%.1f GB)\n", h.MemoryMB, float64(h.MemoryMB)/1024.0)
}

func runValidate(path string) {
	cfg, err := LoadConfig(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(2)
	}
	host, err := DetectHost()
	if err != nil {
		fmt.Fprintf(os.Stderr, "detect: %v\n", err)
		os.Exit(2)
	}
	if err := ValidateBudget(cfg, host); err != nil {
		fmt.Fprintf(os.Stderr, "validate: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("OK: config fits host capacity.")
}

func runRender(path string) {
	cfg, err := LoadConfig(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(2)
	}
	host, err := DetectHost()
	if err != nil {
		fmt.Fprintf(os.Stderr, "detect: %v\n", err)
		os.Exit(2)
	}
	if err := ValidateBudget(cfg, host); err != nil {
		fmt.Fprintf(os.Stderr, "validate: %v\n", err)
		os.Exit(1)
	}
	const composeOut = "deploy/docker-compose.override.yml"
	if err := RenderCompose(cfg, composeOut); err != nil {
		fmt.Fprintf(os.Stderr, "render compose: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s\n", composeOut)
	const helmOut = "deploy/k8s/values-local.yaml"
	if err := RenderHelm(cfg, helmOut); err != nil {
		fmt.Fprintf(os.Stderr, "render helm: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s\n", helmOut)
}
