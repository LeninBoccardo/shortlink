-- +goose Up
CREATE TABLE hits (
    id          BIGSERIAL PRIMARY KEY,
    slug        TEXT NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    country     TEXT,
    device      TEXT
);

CREATE INDEX idx_hits_slug ON hits(slug);

-- +goose Down
DROP TABLE hits;
