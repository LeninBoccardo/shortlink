package main

import (
	"context"
	"log/slog"

	"github.com/leninboccardo/shortlink/internal/events"
	"github.com/leninboccardo/shortlink/internal/keysfile"
)

// attackResult holds the post-attack summary for one key profile. Piece 4
// (next commits) fills in real vegeta metrics.
type attackResult struct {
	Profile keysfile.Entry
}

// runAttacks is the multi-key vegeta attack runner. Piece 4 wires it up;
// the skeleton just returns an empty result set so the skeleton builds.
func runAttacks(_ context.Context, _ *keysfile.File, _ runConfig, _ *events.Emitter, _ *slog.Logger) []attackResult {
	return nil
}

func printSummary(_ []attackResult, _ map[string]int, log *slog.Logger) {
	log.Info("attack summary placeholder — real metrics land in piece 4")
}
