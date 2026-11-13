-- name: ScanTargets :many
WITH head AS (
  SELECT GREATEST(COALESCE(MAX(id), 0), (SELECT purged_through FROM faucet_purge_mark))::bigint AS max_id
  FROM faucet_events
)
SELECT
  s.key_hash, s.event_type, s.arguments, s.repo_id, s.scanned_through,
  (SELECT COUNT(*) FROM faucet_deliveries d WHERE d.key_hash = s.key_hash)::bigint AS pending
FROM faucet_subscriptions s, head
WHERE s.expires_at > sqlc.arg(now)
  AND s.scanned_through < head.max_id
  AND (SELECT COUNT(*) FROM faucet_deliveries d WHERE d.key_hash = s.key_hash) < sqlc.arg(max_pending)::bigint
ORDER BY s.scanned_through, s.key_hash
LIMIT sqlc.arg(row_limit);

-- name: AdvanceScan :execrows
UPDATE faucet_subscriptions
SET scanned_through = sqlc.arg(new_scanned_through)
WHERE key_hash = sqlc.arg(key_hash)
  AND scanned_through = sqlc.arg(expected_scanned_through)
  AND expires_at > sqlc.arg(now);

-- name: InsertDeliveries :exec
INSERT INTO faucet_deliveries (key_hash, event_id, message_id, enqueued_at, next_attempt_at)
SELECT sqlc.arg(key_hash)::bytea, e.event_id, m.message_id, sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz
FROM unnest(sqlc.arg(event_ids)::bigint[]) WITH ORDINALITY AS e(event_id, n)
JOIN unnest(sqlc.arg(message_ids)::text[]) WITH ORDINALITY AS m(message_id, n) USING (n);

-- name: ClaimDelivery :one
WITH due AS (
  SELECT d.key_hash, d.event_id
  FROM faucet_deliveries d
  JOIN faucet_subscriptions s ON s.key_hash = d.key_hash
  WHERE s.expires_at > sqlc.arg(now)
    AND d.next_attempt_at <= sqlc.arg(now)
    AND (d.lease_until IS NULL OR d.lease_until <= sqlc.arg(now))
  ORDER BY d.next_attempt_at, d.event_id
  LIMIT 1
  FOR UPDATE OF d SKIP LOCKED
), claimed AS (
  UPDATE faucet_deliveries d
  SET attempts = d.attempts + 1, lease_until = sqlc.arg(lease_until)
  FROM due
  WHERE d.key_hash = due.key_hash AND d.event_id = due.event_id
  RETURNING d.key_hash, d.event_id, d.message_id, d.attempts, d.enqueued_at
)
SELECT
  c.key_hash, c.event_id, c.message_id, c.attempts, c.enqueued_at,
  s.callback_url, s.secret_enc, s.repo_id,
  (SELECT COALESCE(MIN(p.event_id), c.event_id) - 1 FROM faucet_deliveries p WHERE p.key_hash = c.key_hash)::bigint AS watermark,
  e.id AS stored_event_id, e.source, e.delivery_id, e.type, e.repo_id AS event_repo_id, e.prs, e.head_sha, COALESCE(e.data, 'null'::jsonb)::jsonb AS data, e.received_at
FROM claimed c
JOIN faucet_subscriptions s ON s.key_hash = c.key_hash
LEFT JOIN faucet_events e ON e.id = c.event_id;

-- name: RescheduleDelivery :execrows
UPDATE faucet_deliveries
SET next_attempt_at = sqlc.arg(next_attempt_at), lease_until = NULL
WHERE key_hash = sqlc.arg(key_hash) AND event_id = sqlc.arg(event_id) AND attempts = sqlc.arg(attempt);

-- name: EndDelivery :execrows
DELETE FROM faucet_deliveries
WHERE key_hash = sqlc.arg(key_hash) AND event_id = sqlc.arg(event_id) AND attempts = sqlc.arg(attempt);

-- name: RecordDeliverySuccess :exec
UPDATE faucet_subscriptions
SET last_delivery_at = sqlc.arg(now), last_error = NULL, failed_since = NULL
WHERE key_hash = sqlc.arg(key_hash);

-- name: RecordDeliveryFailure :exec
UPDATE faucet_subscriptions
SET last_error = sqlc.arg(last_error), failed_since = COALESCE(failed_since, sqlc.arg(now))
WHERE key_hash = sqlc.arg(key_hash);

-- name: CountDeliveries :one
SELECT COUNT(*)::bigint AS pending FROM faucet_deliveries WHERE key_hash = sqlc.arg(key_hash);

-- name: DeleteSubscriptionIfSealed :execrows
DELETE FROM faucet_subscriptions WHERE key_hash = sqlc.arg(key_hash) AND secret_enc = sqlc.arg(secret_enc);

-- name: ExpireDeliveries :execrows
DELETE FROM faucet_deliveries d
USING faucet_subscriptions s
WHERE s.key_hash = d.key_hash
  AND s.expires_at <= sqlc.arg(now)
  AND (d.lease_until IS NULL OR d.lease_until <= sqlc.arg(now));

-- name: LockSubscription :one
SELECT 1::int AS locked FROM faucet_subscriptions WHERE key_hash = sqlc.arg(key_hash) FOR NO KEY UPDATE;
