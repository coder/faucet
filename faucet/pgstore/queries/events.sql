-- name: LockEvents :exec
SELECT pg_advisory_xact_lock(sqlc.arg(key)::bigint);

-- name: InsertEvent :one
INSERT INTO faucet_events (source, delivery_id, type, repo_id, prs, head_sha, data)
VALUES (
  sqlc.arg(source),
  sqlc.arg(delivery_id),
  sqlc.arg(type),
  sqlc.arg(repo_id),
  sqlc.arg(prs)::int[],
  NULLIF(sqlc.arg(head_sha)::text, ''),
  sqlc.arg(data)
)
ON CONFLICT (delivery_id) DO NOTHING
RETURNING *;

-- name: GetEventByDelivery :one
SELECT * FROM faucet_events WHERE delivery_id = $1;

-- name: EventsAfter :many
SELECT * FROM faucet_events
WHERE id > sqlc.arg(after_id)
ORDER BY id
LIMIT sqlc.arg(row_limit);
