-- +goose Up
-- short_urls.slug was created with `TEXT UNIQUE` in migration 002, which
-- already produced an implicit btree index. The subsequent explicit
-- `CREATE INDEX idx_short_urls_slug ON short_urls(slug)` was a duplicate
-- and doubled per-row write amplification for no read benefit (the unique
-- constraint serves redirects equally well).
DROP INDEX IF EXISTS idx_short_urls_slug;

-- +goose Down
CREATE INDEX IF NOT EXISTS idx_short_urls_slug ON short_urls(slug);
