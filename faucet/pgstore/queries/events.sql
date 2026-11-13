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

-- name: PollEvents :many
WITH mark AS (
  SELECT purged_through FROM faucet_purge_mark
), head AS (
  SELECT GREATEST(COALESCE(MAX(id), 0), (SELECT purged_through FROM mark))::bigint AS max_id
  FROM faucet_events
), page AS (
  SELECT e.* FROM faucet_events e
  WHERE e.repo_id = sqlc.arg(repo_id)
    AND e.id > sqlc.arg(after_id)
    AND (cardinality(sqlc.arg(types)::text[]) = 0 OR e.type = ANY(sqlc.arg(types)::text[]))
    AND (sqlc.arg(head_sha)::text = '' OR e.head_sha = sqlc.arg(head_sha)::text)
    AND (sqlc.arg(pr)::int = 0 OR sqlc.arg(pr)::int = ANY(e.prs))
  ORDER BY e.id
  LIMIT sqlc.arg(row_limit)
)
SELECT
  head.max_id,
  (SELECT purged_through FROM mark)::bigint AS purged_through,
  page.id, page.source, page.delivery_id, page.type, page.repo_id, page.prs, page.head_sha,
  COALESCE(page.data, 'null'::jsonb)::jsonb AS data,
  page.received_at
FROM head LEFT JOIN page ON true
ORDER BY page.id;

-- name: CursorAt :one
SELECT GREATEST(COALESCE(MAX(id), 0), (SELECT purged_through FROM faucet_purge_mark))::bigint AS cursor
FROM faucet_events
WHERE received_at < sqlc.arg(before);

-- name: LockPurgeMark :exec
SELECT purged_through FROM faucet_purge_mark FOR UPDATE;

-- name: DeleteEventsBefore :one
WITH deleted AS (
  DELETE FROM faucet_events WHERE received_at < sqlc.arg(before) RETURNING id
)
SELECT COUNT(*)::bigint AS deleted, COALESCE(MAX(id), 0)::bigint AS max_id FROM deleted;

-- name: RaisePurgeMark :one
UPDATE faucet_purge_mark
SET purged_through = GREATEST(purged_through, sqlc.arg(through)::bigint)
RETURNING purged_through;

-- name: StoredDeliveries :many
SELECT delivery_id FROM faucet_events WHERE delivery_id = ANY(sqlc.arg(delivery_ids)::text[]);
