// Package migrations embeds the goose SQL migration files into the binary so
// cmd/migrate can apply them without depending on the source tree at runtime.
package migrations

import "embed"

// FS holds every migration file in this directory. cmd/migrate hands it to
// goose via goose.SetBaseFS.
//
//go:embed *.sql
var FS embed.FS
