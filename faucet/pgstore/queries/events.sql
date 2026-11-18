-- name: LockEvents :exec
SELECT pg_advisory_xact_lock(sqlc.arg(key)::bigint);

-- name: InsertEvent :one
INSERT INTO faucet_events (source, delivery_id, type, repo_id, owner_id, repo_name, prs, head_sha, data, received_at)
VALUES (
  sqlc.arg(source),
  sqlc.arg(delivery_id),
  sqlc.arg(type),
  sqlc.arg(repo_id),
  sqlc.arg(owner_id),
  sqlc.arg(repo_name),
  sqlc.arg(prs)::int[],
  NULLIF(sqlc.arg(head_sha)::text, ''),
  sqlc.arg(data)::text::jsonb,
  clock_timestamp()
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
-- Each kind of target has a branch of its own, so each reads its own index:
-- faucet_events_poll for a repository, faucet_events_owner for an owner, and
-- the primary key for all. An event stored before owners were recorded has
-- owner 0, which no owner target names.
WITH mark AS (
  SELECT purged_through FROM faucet_purge_mark
), head AS (
  SELECT GREATEST(COALESCE(MAX(id), 0), (SELECT purged_through FROM mark))::bigint AS max_id
  FROM faucet_events
), page AS (
  SELECT e.* FROM faucet_events e
  WHERE (
      (sqlc.arg(target_kind)::text = 'repo' AND e.repo_id = sqlc.arg(repo_id)::bigint)
      OR (sqlc.arg(target_kind)::text = 'owner' AND e.owner_id = sqlc.arg(owner_id)::bigint AND e.owner_id > 0)
      OR sqlc.arg(target_kind)::text = 'all'
    )
    AND (cardinality(sqlc.arg(repo_names)::text[]) = 0 OR lower(e.repo_name) = ANY(sqlc.arg(repo_names)::text[]))
    AND e.id > sqlc.arg(after_id)
    AND (cardinality(sqlc.arg(types)::text[]) = 0 OR e.type = ANY(sqlc.arg(types)::text[]))
    AND (sqlc.arg(head_sha)::text = '' OR e.head_sha = sqlc.arg(head_sha)::text)
    AND (sqlc.arg(pr)::int = 0 OR sqlc.arg(pr)::int = ANY(e.prs))
    AND NOT EXISTS (
      SELECT 1 FROM jsonb_to_recordset(sqlc.arg(fields)::text::jsonb) AS f(field text, matches text[], types text[])
      WHERE e.type = ANY(f.types) AND NOT COALESCE(e.data ->> f.field = ANY(f.matches), false)
    )
  ORDER BY e.id
  LIMIT sqlc.arg(row_limit)
)
SELECT
  head.max_id,
  (SELECT purged_through FROM mark)::bigint AS purged_through,
  page.id, page.source, page.delivery_id, page.type, page.repo_id, page.owner_id, page.repo_name, page.prs, page.head_sha,
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

-- name: AdvanceSubscriptionsThatLostNothing :execrows
-- A subscription moves only through the branch of its kind, so one of a kind
-- that this build does not know stays where it is: a false gap is safe, a
-- suppressed one is not. Like ScanTargets, it ignores the subscription's
-- other filters, so it can leave a subscription below the mark that lost
-- nothing it wanted, and never moves one that lost something.
WITH doomed AS (
  SELECT COALESCE(MAX(ev.id), 0)::bigint AS mark FROM faucet_events ev WHERE ev.received_at < sqlc.arg(before)
)
UPDATE faucet_subscriptions s
SET scanned_through = doomed.mark
FROM doomed
WHERE s.scanned_through < doomed.mark
  AND s.scanned_through >= (SELECT purged_through FROM faucet_purge_mark)
  AND (
    (s.target_kind = 'repo' AND NOT EXISTS (
      SELECT 1 FROM faucet_events e
      WHERE e.repo_id = s.repo_id AND e.type = ANY(s.event_types) AND e.id > s.scanned_through AND e.id <= doomed.mark
    ))
    OR (s.target_kind = 'owner' AND NOT EXISTS (
      SELECT 1 FROM faucet_events e
      WHERE e.owner_id = s.owner_id AND e.owner_id > 0 AND e.type = ANY(s.event_types) AND e.id > s.scanned_through AND e.id <= doomed.mark
    ))
    OR (s.target_kind = 'all' AND NOT EXISTS (
      SELECT 1 FROM faucet_events e
      WHERE e.type = ANY(s.event_types) AND e.id > s.scanned_through AND e.id <= doomed.mark
    ))
  );

-- name: RaisePurgeMark :one
UPDATE faucet_purge_mark
SET purged_through = GREATEST(purged_through, sqlc.arg(through)::bigint)
RETURNING purged_through;
-- name: LockSubscriptions :exec
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(principal)::text, 0));

-- name: DeleteExpiredSubscriptionsOf :exec
DELETE FROM faucet_subscriptions WHERE principal = sqlc.arg(principal) AND expires_at <= sqlc.arg(now);

-- name: CountOtherSubscriptions :one
SELECT COUNT(*)::bigint AS others FROM faucet_subscriptions WHERE principal = sqlc.arg(principal) AND key_hash <> sqlc.arg(key_hash);

-- name: UpsertSubscription :one
INSERT INTO faucet_subscriptions (key_hash, principal, callback_url, event_type, event_types, arguments, target_kind, repo_id, owner_id, secret_enc, cursor, scanned_through, created_at, updated_at, expires_at)
VALUES (
  sqlc.arg(key_hash),
  sqlc.arg(principal),
  sqlc.arg(callback_url),
  sqlc.arg(event_type),
  sqlc.arg(event_types)::text[],
  sqlc.arg(arguments)::text::jsonb,
  sqlc.arg(target_kind),
  sqlc.arg(repo_id),
  sqlc.arg(owner_id),
  sqlc.arg(secret_enc),
  sqlc.arg(cursor),
  sqlc.arg(cursor),
  sqlc.arg(now),
  sqlc.arg(now),
  sqlc.arg(expires_at)
)
ON CONFLICT (key_hash) DO UPDATE SET
  event_types = EXCLUDED.event_types,
  target_kind = EXCLUDED.target_kind,
  repo_id = EXCLUDED.repo_id,
  owner_id = EXCLUDED.owner_id,
  previous_secret_enc = CASE
    WHEN faucet_subscriptions.secret_enc = EXCLUDED.secret_enc THEN faucet_subscriptions.previous_secret_enc
    WHEN sqlc.narg(previous_secret_until)::timestamptz IS NULL THEN NULL
    ELSE faucet_subscriptions.secret_enc
  END,
  previous_secret_until = CASE
    WHEN faucet_subscriptions.secret_enc = EXCLUDED.secret_enc THEN faucet_subscriptions.previous_secret_until
    ELSE sqlc.narg(previous_secret_until)::timestamptz
  END,
  secret_enc = EXCLUDED.secret_enc,
  updated_at = EXCLUDED.updated_at,
  expires_at = EXCLUDED.expires_at
RETURNING *;

-- name: DeleteSubscription :execrows
DELETE FROM faucet_subscriptions WHERE key_hash = sqlc.arg(key_hash) AND principal = sqlc.arg(principal);

-- name: DeleteLiveSubscription :one
DELETE FROM faucet_subscriptions
WHERE key_hash = sqlc.arg(key_hash) AND principal = sqlc.arg(principal) AND expires_at > sqlc.arg(now)
RETURNING *;

-- name: DeleteSubscriptionsOf :execrows
DELETE FROM faucet_subscriptions WHERE principal = sqlc.arg(principal);

-- name: DeleteExpiredSubscriptions :execrows
DELETE FROM faucet_subscriptions WHERE expires_at <= sqlc.arg(now);

-- name: ClearExpiredPreviousSecrets :execrows
UPDATE faucet_subscriptions
SET previous_secret_enc = NULL, previous_secret_until = NULL
WHERE previous_secret_until <= sqlc.arg(now)::timestamptz;
