-- +goose NO TRANSACTION
-- +goose Up
-- short_urls.slug was created with `TEXT UNIQUE` in migration 002, which
-- already produced an implicit btree index. The subsequent explicit
-- `CREATE INDEX idx_short_urls_slug ON short_urls(slug)` was a duplicate
-- and doubled per-row write amplification for no read benefit (the unique
-- constraint serves redirects equally well).
--
-- CONCURRENTLY avoids the brief AccessExclusiveLock that a plain DROP INDEX
-- would take on short_urls — important for a hot redirect/insert table.
-- CONCURRENTLY cannot run inside a transaction, hence the `NO TRANSACTION`
-- directive at the top of the file. Idempotent (IF EXISTS) so re-applying
-- on a DB that already ran the original non-concurrent variant is a no-op.
DROP INDEX CONCURRENTLY IF EXISTS idx_short_urls_slug;

-- +goose Down
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_short_urls_slug ON short_urls(slug);
