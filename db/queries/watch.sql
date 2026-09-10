-- name: Checkpoint :one
SELECT * FROM checkpoint WHERE id = 1;

-- name: Initialize :exec
INSERT INTO checkpoint (id, cursor, anchor, source) VALUES (1, ?, ?, ?);

-- name: Advance :exec
UPDATE checkpoint SET cursor = ?, anchor = ?, source = ?, drain_discovered = 0 WHERE id = 1;

-- name: Discover :exec
INSERT INTO pending (path, source_id, source) VALUES (?, ?, ?)
ON CONFLICT (path) DO UPDATE SET
attempts = CASE WHEN pending.expired = 1 AND pending.source = excluded.source AND pending.source_id != excluded.source_id
THEN 0 ELSE pending.attempts END,
first_fail = CASE WHEN pending.expired = 1 AND pending.source = excluded.source AND pending.source_id != excluded.source_id
THEN 0 ELSE pending.first_fail END,
next_at = CASE WHEN pending.expired = 1 AND pending.source = excluded.source AND pending.source_id != excluded.source_id
THEN 0 ELSE pending.next_at END,
final_generation = CASE WHEN pending.expired = 1 AND pending.source = excluded.source AND pending.source_id != excluded.source_id
THEN 0 ELSE pending.final_generation END,
expired = CASE WHEN pending.expired = 1 AND pending.source = excluded.source AND pending.source_id != excluded.source_id
THEN 0 ELSE pending.expired END,
source_id = excluded.source_id, source = excluded.source;

-- name: Fresh :many
SELECT * FROM pending WHERE expired = 0 AND attempts = 0 ORDER BY source_id, path LIMIT ?;

-- name: Due :many
SELECT * FROM pending WHERE expired = 0 AND attempts > 0 AND next_at <= ?
ORDER BY next_at, source_id, path LIMIT ?;

-- name: Final :many
SELECT * FROM pending WHERE expired = 0 AND final_generation < ?
ORDER BY final_generation, source_id, path LIMIT ?;

-- name: MarkFinal :exec
UPDATE pending SET final_generation = ? WHERE path = ?;

-- name: BeginDrain :exec
UPDATE checkpoint SET
boundary = max(COALESCE(boundary, 0), CAST(sqlc.arg(boundary) AS INTEGER)),
generation = generation + CASE WHEN boundary IS NULL THEN 1 ELSE 0 END,
drain_discovered = CASE WHEN boundary IS NULL OR boundary < CAST(sqlc.arg(boundary) AS INTEGER) THEN 0 ELSE drain_discovered END
WHERE id = 1;

-- name: EndDrain :exec
UPDATE checkpoint SET boundary = NULL, last_expired = expired, expired = 0,
last_generation = generation, last_boundary = boundary WHERE id = 1;

-- name: Retry :exec
UPDATE pending SET attempts = attempts + 1,
first_fail = CASE WHEN first_fail = 0 THEN sqlc.arg(now) ELSE first_fail END,
next_at = sqlc.arg(next_at) WHERE path = sqlc.arg(path);

-- name: Retire :exec
DELETE FROM pending WHERE path = ?;

-- name: Expire :exec
UPDATE checkpoint SET expired = expired + 1 WHERE id = 1;

-- name: CountPending :one
SELECT count(*) FROM pending WHERE expired = 0;

-- name: Pending :one
SELECT * FROM pending WHERE path = ?;

-- name: HasPending :one
SELECT CAST(EXISTS(SELECT 1 FROM pending WHERE expired = 0) AS INTEGER);

-- name: Discovered :exec
UPDATE checkpoint SET drain_discovered = 1 WHERE id = 1;

-- name: ReconcileBoundary :exec
UPDATE checkpoint SET boundary = ? WHERE id = 1 AND boundary IS NOT NULL;

-- name: MarkExpired :exec
UPDATE pending SET expired = 1 WHERE path = ?;

-- name: PendingPage :many
SELECT * FROM pending WHERE path > ? ORDER BY path LIMIT ?;
