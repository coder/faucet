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

-- name: ScanEvents :many
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
    AND e.id <= sqlc.arg(through)::bigint
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
  head.max_id AS head,
  (SELECT purged_through FROM mark)::bigint AS purged_through,
  page.id, page.source, page.delivery_id, page.type, page.repo_id, page.owner_id, page.repo_name, page.prs, page.head_sha,
  COALESCE(page.data, 'null'::jsonb)::jsonb AS data,
  page.received_at, page.repo_seq, page.owner_seq
FROM head LEFT JOIN page ON true
ORDER BY page.id;

-- name: PollLine :many
-- A read of the line of one target by position: repo_seq on
-- faucet_events_repo_line for a repository, owner_seq on
-- faucet_events_owner_line for an owner, and the ID on the primary key for
-- all. A page is in the order of its line, which is the order of its IDs,
-- so the index of the line also gives the order and the read stops at the
-- limit.
-- The head and the purge mark are the line's, read in the same snapshot. The
-- head is the higher of the newest position and the purge mark, which a
-- purge of every event of the line leaves.
WITH line AS (
  SELECT
    GREATEST(
      CASE sqlc.arg(target_kind)::text
        WHEN 'repo' THEN (SELECT MAX(h.repo_seq) FROM faucet_events h WHERE h.repo_id = sqlc.arg(repo_id)::bigint)
        WHEN 'owner' THEN (SELECT MAX(h.owner_seq) FROM faucet_events h WHERE h.owner_id = sqlc.arg(owner_id)::bigint AND h.owner_id > 0)
        ELSE (SELECT MAX(h.id) FROM faucet_events h)
      END,
      faucet_line_purged_through(sqlc.arg(target_kind)::text, sqlc.arg(repo_id)::bigint, sqlc.arg(owner_id)::bigint),
      0
    )::bigint AS head,
    faucet_line_purged_through(sqlc.arg(target_kind)::text, sqlc.arg(repo_id)::bigint, sqlc.arg(owner_id)::bigint)::bigint AS purged_through
), page AS (
  SELECT e.* FROM faucet_events e
  WHERE (
      (sqlc.arg(target_kind)::text = 'repo' AND e.repo_id = sqlc.arg(repo_id)::bigint
        AND e.repo_seq > sqlc.arg(after)::bigint AND e.repo_seq <= sqlc.arg(through)::bigint)
      OR (sqlc.arg(target_kind)::text = 'owner' AND e.owner_id = sqlc.arg(owner_id)::bigint AND e.owner_id > 0
        AND e.owner_seq > sqlc.arg(after)::bigint AND e.owner_seq <= sqlc.arg(through)::bigint)
      OR (sqlc.arg(target_kind)::text = 'all'
        AND e.id > sqlc.arg(after)::bigint AND e.id <= sqlc.arg(through)::bigint)
    )
    AND (cardinality(sqlc.arg(repo_names)::text[]) = 0 OR lower(e.repo_name) = ANY(sqlc.arg(repo_names)::text[]))
    AND (cardinality(sqlc.arg(types)::text[]) = 0 OR e.type = ANY(sqlc.arg(types)::text[]))
    AND (sqlc.arg(head_sha)::text = '' OR e.head_sha = sqlc.arg(head_sha)::text)
    AND (sqlc.arg(pr)::int = 0 OR sqlc.arg(pr)::int = ANY(e.prs))
    AND NOT EXISTS (
      SELECT 1 FROM jsonb_to_recordset(sqlc.arg(fields)::text::jsonb) AS f(field text, matches text[], types text[])
      WHERE e.type = ANY(f.types) AND NOT COALESCE(e.data ->> f.field = ANY(f.matches), false)
    )
  ORDER BY CASE sqlc.arg(target_kind)::text WHEN 'repo' THEN e.repo_seq WHEN 'owner' THEN e.owner_seq ELSE e.id END
  LIMIT sqlc.arg(row_limit)
)
SELECT
  line.head,
  line.purged_through,
  page.id, page.source, page.delivery_id, page.type, page.repo_id, page.owner_id, page.repo_name, page.prs, page.head_sha,
  COALESCE(page.data, 'null'::jsonb)::jsonb AS data,
  page.received_at, page.repo_seq, page.owner_seq
FROM line LEFT JOIN page ON true
ORDER BY page.id;

-- name: PositionAt :one
SELECT GREATEST(
  CASE sqlc.arg(target_kind)::text
    WHEN 'repo' THEN (SELECT MAX(e.repo_seq) FROM faucet_events e WHERE e.repo_id = sqlc.arg(repo_id)::bigint AND e.received_at < sqlc.arg(before))
    WHEN 'owner' THEN (SELECT MAX(e.owner_seq) FROM faucet_events e WHERE e.owner_id = sqlc.arg(owner_id)::bigint AND e.owner_id > 0 AND e.received_at < sqlc.arg(before))
    ELSE (SELECT MAX(e.id) FROM faucet_events e WHERE e.received_at < sqlc.arg(before))
  END,
  faucet_line_purged_through(sqlc.arg(target_kind)::text, sqlc.arg(repo_id)::bigint, sqlc.arg(owner_id)::bigint),
  0
)::bigint AS position;

-- name: StartSubscription :one
-- One statement, so one snapshot: a purge that commits alongside it either
-- raised the purge mark that floor reads, or commits after it and leaves after
-- below the new purge mark of faucet_purge_mark, where the delivery scan
-- reports a gap. floor reads the receive times only when since is given,
-- on the line's index from its head down to since; with no since it is the
-- line's purge mark alone. next is the ID of the target's first event above
-- the start, on its line's index. The highest event ID is read in the same
-- snapshot as the head of the line, so an event of the target above that head
-- commits with a higher ID.
WITH line AS (
  SELECT
    GREATEST(
      CASE sqlc.arg(target_kind)::text
        WHEN 'repo' THEN (SELECT MAX(h.repo_seq) FROM faucet_events h WHERE h.repo_id = sqlc.arg(repo_id)::bigint)
        WHEN 'owner' THEN (SELECT MAX(h.owner_seq) FROM faucet_events h WHERE h.owner_id = sqlc.arg(owner_id)::bigint AND h.owner_id > 0)
        ELSE (SELECT MAX(h.id) FROM faucet_events h)
      END,
      faucet_line_purged_through(sqlc.arg(target_kind)::text, sqlc.arg(repo_id)::bigint, sqlc.arg(owner_id)::bigint),
      0
    )::bigint AS head,
    GREATEST(
      CASE WHEN sqlc.narg(since)::timestamptz IS NOT NULL THEN
        CASE sqlc.arg(target_kind)::text
          WHEN 'repo' THEN (SELECT MAX(e.repo_seq) FROM faucet_events e WHERE e.repo_id = sqlc.arg(repo_id)::bigint AND e.received_at < sqlc.narg(since)::timestamptz)
          WHEN 'owner' THEN (SELECT MAX(e.owner_seq) FROM faucet_events e WHERE e.owner_id = sqlc.arg(owner_id)::bigint AND e.owner_id > 0 AND e.received_at < sqlc.narg(since)::timestamptz)
          ELSE (SELECT MAX(e.id) FROM faucet_events e WHERE e.received_at < sqlc.narg(since)::timestamptz)
        END
      END,
      faucet_line_purged_through(sqlc.arg(target_kind)::text, sqlc.arg(repo_id)::bigint, sqlc.arg(owner_id)::bigint),
      0
    )::bigint AS floor
), start AS (
  SELECT
    LEAST(GREATEST(COALESCE(sqlc.narg(cursor)::bigint, 0), line.floor), line.head)::bigint AS position,
    COALESCE(sqlc.narg(cursor)::bigint < line.floor, false)::boolean AS truncated,
    line.head
  FROM line
), log AS (
  SELECT
    GREATEST(COALESCE(MAX(id), 0), (SELECT purged_through FROM faucet_purge_mark))::bigint AS head,
    (SELECT purged_through FROM faucet_purge_mark)::bigint AS purged_through
  FROM faucet_events
), next AS (
  SELECT CASE sqlc.arg(target_kind)::text
    WHEN 'repo' THEN (
      SELECT e.id FROM faucet_events e
      WHERE e.repo_id = sqlc.arg(repo_id)::bigint AND e.repo_seq > (SELECT position FROM start)
      ORDER BY e.repo_seq LIMIT 1)
    WHEN 'owner' THEN (
      SELECT e.id FROM faucet_events e
      WHERE e.owner_id = sqlc.arg(owner_id)::bigint AND e.owner_id > 0 AND e.owner_seq > (SELECT position FROM start)
      ORDER BY e.owner_seq LIMIT 1)
    ELSE (SELECT MIN(e.id) FROM faucet_events e WHERE e.id > (SELECT position FROM start))
  END AS id
)
SELECT
  start.position,
  start.truncated,
  (CASE
    WHEN start.position >= start.head OR next.id IS NULL THEN log.head
    ELSE GREATEST(next.id - 1, log.purged_through)
  END)::bigint AS after
FROM start, log, next;

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
INSERT INTO faucet_subscriptions (key_hash, principal, callback_url, event_type, event_types, arguments, target_kind, repo_id, owner_id, secret_enc, cursor, scanned_through, position, created_at, updated_at, expires_at)
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
  sqlc.arg(position),
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
  -- A renewal that moves the subscription to another repository or owner
  -- keeps its cursor, so its position is the new line's at that cursor.
  position = CASE
    WHEN (faucet_subscriptions.target_kind, faucet_subscriptions.repo_id, faucet_subscriptions.owner_id)
      = (EXCLUDED.target_kind, EXCLUDED.repo_id, EXCLUDED.owner_id) THEN faucet_subscriptions.position
    ELSE faucet_line_position(EXCLUDED.target_kind, EXCLUDED.repo_id, EXCLUDED.owner_id, faucet_subscriptions.cursor)
  END,
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
