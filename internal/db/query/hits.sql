-- name: InsertHit :exec
INSERT INTO hits (slug, country, device)
VALUES ($1, $2, $3);
