package auth

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/leninboccardo/shortlink/internal/db"
)

// ErrInvalidKey is returned when an API key is missing, unknown, or revoked.
var ErrInvalidKey = errors.New("invalid api key")

// Validator authenticates raw API keys against the api_keys table.
type Validator struct {
	q *db.Queries
}

// NewValidator builds a Validator over the given query set.
func NewValidator(q *db.Queries) *Validator {
	return &Validator{q: q}
}

// Validate hashes raw and looks it up. A missing or revoked key — both yield
// pgx.ErrNoRows from the query — is reported as ErrInvalidKey.
func (v *Validator) Validate(ctx context.Context, raw string) (db.ApiKey, error) {
	if raw == "" {
		return db.ApiKey{}, ErrInvalidKey
	}
	key, err := v.q.GetAPIKeyByHash(ctx, HashKey(raw))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.ApiKey{}, ErrInvalidKey
		}
		return db.ApiKey{}, err
	}
	return key, nil
}
