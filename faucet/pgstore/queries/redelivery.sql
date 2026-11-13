-- name: EnsureRedeliveryScan :exec
INSERT INTO faucet_redelivery_scan (id, hook, epoch)
VALUES (true, sqlc.arg(hook), 0)
ON CONFLICT (id) DO NOTHING;

-- name: LockRedeliveryScan :one
SELECT hook, epoch, lease_until, top_at FROM faucet_redelivery_scan FOR UPDATE;

-- name: CloseOpenRedeliveryRanges :many
UPDATE faucet_redelivery_ranges
SET state = 'gap', closed_at = sqlc.arg(now)::timestamptz
WHERE state = 'open'
RETURNING older_at, newer_at;

-- name: TakeRedeliveryLease :one
UPDATE faucet_redelivery_scan
SET hook = sqlc.arg(hook),
    epoch = epoch + 1,
    lease_until = sqlc.arg(lease_until)::timestamptz,
    top_at = CASE WHEN sqlc.arg(reset_top)::boolean THEN NULL ELSE top_at END
RETURNING epoch, top_at;

-- name: ListOpenRedeliveryRanges :many
SELECT id, newer_at, older_at, cursor
FROM faucet_redelivery_ranges
WHERE state = 'open'
ORDER BY newer_at DESC, id DESC;

-- name: ReleaseRedeliveryLease :exec
UPDATE faucet_redelivery_scan SET lease_until = NULL WHERE epoch = sqlc.arg(epoch);

-- name: TrackFailedAttempts :one
WITH input AS (
  SELECT unnest(sqlc.arg(guids)::text[]) AS guid,
         unnest(sqlc.arg(attempt_ids)::bigint[]) AS attempt_id,
         unnest(sqlc.arg(delivered_ats)::timestamptz[]) AS delivered_at
), best AS (
  SELECT guid,
         MIN(delivered_at) AS first_at,
         MAX(delivered_at) AS last_at,
         (ARRAY_AGG(attempt_id ORDER BY delivered_at DESC, attempt_id DESC))[1] AS last_id
  FROM input
  GROUP BY guid
), upserted AS (
  INSERT INTO faucet_redeliveries AS r (guid, last_attempt_id, last_attempt_at, first_attempt_at, next_ask_at)
  SELECT guid, last_id, last_at, first_at, sqlc.arg(now)::timestamptz FROM best
  ON CONFLICT (guid) DO UPDATE SET
    last_attempt_id = CASE
      WHEN (EXCLUDED.last_attempt_at, EXCLUDED.last_attempt_id) > (r.last_attempt_at, r.last_attempt_id)
      THEN EXCLUDED.last_attempt_id ELSE r.last_attempt_id END,
    last_attempt_at = GREATEST(r.last_attempt_at, EXCLUDED.last_attempt_at),
    first_attempt_at = LEAST(r.first_attempt_at, EXCLUDED.first_attempt_at)
  WHERE r.state = 'pending'
  RETURNING (xmax = 0) AS inserted
)
SELECT COUNT(*) FILTER (WHERE inserted)::bigint AS tracked FROM upserted;

-- name: RecoverSucceededDeliveries :many
UPDATE faucet_redeliveries
SET state = 'recovered', settled_at = sqlc.arg(now)::timestamptz
WHERE guid = ANY(sqlc.arg(guids)::text[]) AND state = 'pending'
RETURNING guid;

-- name: RaiseRedeliveryTop :exec
UPDATE faucet_redelivery_scan SET top_at = GREATEST(top_at, sqlc.arg(top)::timestamptz);

-- name: InsertRedeliveryRange :one
INSERT INTO faucet_redelivery_ranges (newer_at, older_at, cursor)
VALUES (sqlc.arg(newer_at), sqlc.arg(older_at), sqlc.arg(cursor))
RETURNING id;

-- name: AdvanceRedeliveryRange :execrows
UPDATE faucet_redelivery_ranges
SET newer_at = sqlc.arg(newer_at), cursor = sqlc.arg(cursor)
WHERE id = sqlc.arg(id) AND state = 'open';

-- name: FinishRedeliveryRange :execrows
DELETE FROM faucet_redelivery_ranges WHERE id = sqlc.arg(id) AND state = 'open';

-- name: GapRedeliveryRange :execrows
UPDATE faucet_redelivery_ranges
SET state = 'gap', older_at = sqlc.arg(from_at), newer_at = sqlc.arg(to_at), cursor = '', closed_at = sqlc.arg(now)::timestamptz
WHERE id = sqlc.arg(id) AND state = 'open';

-- name: SettleRecoveredRedeliveries :many
UPDATE faucet_redeliveries r
SET state = 'recovered', settled_at = sqlc.arg(now)::timestamptz
WHERE r.state = 'pending'
  AND EXISTS (SELECT 1 FROM faucet_events e WHERE e.delivery_id = r.guid)
RETURNING r.guid;

-- name: SettleExpiredRedeliveries :many
UPDATE faucet_redeliveries
SET state = 'gave_up', reason = 'expired', settled_at = sqlc.arg(now)::timestamptz
WHERE state = 'pending' AND first_attempt_at <= sqlc.arg(expired_before)::timestamptz
RETURNING guid, asks, first_attempt_at;

-- name: SettleLimitedRedeliveries :many
UPDATE faucet_redeliveries
SET state = 'gave_up', reason = 'limit', settled_at = sqlc.arg(now)::timestamptz
WHERE state = 'pending' AND asks >= sqlc.arg(max_asks)::integer AND next_ask_at <= sqlc.arg(now)::timestamptz
RETURNING guid, asks, first_attempt_at;

-- name: CountPendingRedeliveries :one
SELECT COUNT(*)::bigint AS pending FROM faucet_redeliveries WHERE state = 'pending';

-- name: ClaimRedelivery :one
WITH due AS (
  SELECT r.guid
  FROM faucet_redeliveries r
  WHERE r.state = 'pending'
    AND r.asks < sqlc.arg(max_asks)::integer
    AND r.next_ask_at <= sqlc.arg(now)::timestamptz
    AND r.first_attempt_at > sqlc.arg(not_before)::timestamptz
    AND NOT EXISTS (SELECT 1 FROM faucet_events e WHERE e.delivery_id = r.guid)
  ORDER BY r.next_ask_at, r.first_attempt_at, r.guid
  LIMIT 1
  FOR UPDATE OF r SKIP LOCKED
)
UPDATE faucet_redeliveries r
SET asks = r.asks + 1,
    next_ask_at = sqlc.arg(now)::timestamptz
      + make_interval(secs => (sqlc.arg(schedule_secs)::float8[])[r.asks + 1])
FROM due
WHERE r.guid = due.guid
RETURNING r.guid, r.last_attempt_id, r.asks;

-- name: PurgeRedeliveries :one
WITH purged AS (
  DELETE FROM faucet_redeliveries
  WHERE state <> 'pending' AND GREATEST(last_attempt_at, settled_at) < sqlc.arg(before)::timestamptz
  RETURNING 1
), purged_gaps AS (
  DELETE FROM faucet_redelivery_ranges
  WHERE state = 'gap' AND closed_at < sqlc.arg(before)::timestamptz
  RETURNING 1
)
SELECT ((SELECT COUNT(*) FROM purged) + (SELECT COUNT(*) FROM purged_gaps))::bigint AS deleted;
