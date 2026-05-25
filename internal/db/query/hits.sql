-- name: RecordHit :exec
-- Single round-trip per redirect: inserts the hits row and bumps the
-- short_urls counter in one statement (the CTE runs the insert; the outer
-- UPDATE bumps the counter). Replaces the prior InsertHit + IncrementHitCount
-- pair, halving the PG round-trips on the redirect path. Named params dodge
-- the slug-column ambiguity between hits.slug and short_urls.slug.
WITH ins AS (
    INSERT INTO hits (slug, country, device)
    VALUES (@slug, @country, @device)
)
UPDATE short_urls SET hit_count = hit_count + 1 WHERE short_urls.slug = @slug;
